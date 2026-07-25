// Package cache 实现流式缓存：首次拉流时边下边服务（tee），
// 后续请求直接命中本地文件。凭据只在 Resolve/拉流的服务端路径上使用。
package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

var ErrNotFound = errors.New("track not cached")

type Cache struct {
	dir      string
	maxBytes int64
	st       *store.Store
	reg      *provider.Registry

	client *http.Client

	mu       sync.Mutex
	inflight map[provider.TrackRef]*download
	history  []DownloadStatus // 最近完成的下载（最新在前，环形上限 historyCap）
}

// download 表示一次进行中的拉取；跟随者等待完成后读缓存文件。
type download struct {
	done      chan struct{}
	err       error
	startedAt time.Time
	total     int64 // 上游 Content-Length；-1 = 未知
	fetched   atomic.Int64
}

// DownloadStatus 是一次下载的可观测快照（进行中或历史）。
type DownloadStatus struct {
	TrackRef   string `json:"track_ref"`
	Fetched    int64  `json:"fetched_bytes"`
	Total      int64  `json:"total_bytes"` // -1 = 未知
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	Status     string `json:"status"` // downloading | ok | failed
	Error      string `json:"error,omitempty"`
}

const historyCap = 20

func New(dir string, maxBytes int64, st *store.Store, reg *provider.Registry) *Cache {
	// 清理上次进程退出时遗留的临时下载文件
	if matches, _ := filepath.Glob(filepath.Join(dir, "dl-*")); len(matches) > 0 {
		for _, m := range matches {
			os.Remove(m)
		}
		log.Printf("[cache] removed %d stale temp files", len(matches))
	}
	return &Cache{
		dir:      dir,
		maxBytes: maxBytes,
		st:       st,
		reg:      reg,
		client:   &http.Client{Timeout: 0}, // 流式拉取，不设总超时；由 ctx 控制
		inflight: map[provider.TrackRef]*download{},
	}
}

// Downloads 返回进行中的下载快照。
func (c *Cache) Downloads() []DownloadStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []DownloadStatus{}
	for ref, dl := range c.inflight {
		out = append(out, DownloadStatus{
			TrackRef:  ref.String(),
			Fetched:   dl.fetched.Load(),
			Total:     dl.total,
			StartedAt: dl.startedAt.UnixMilli(),
			Status:    "downloading",
		})
	}
	return out
}

// History 返回最近完成的下载记录（最新在前）。
func (c *Cache) History() []DownloadStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]DownloadStatus, len(c.history))
	copy(out, c.history)
	return out
}

// recordHistory 登记一条完成的下载记录。
func (c *Cache) recordHistory(ref provider.TrackRef, dl *download, err error) {
	rec := DownloadStatus{
		TrackRef:   ref.String(),
		Fetched:    dl.fetched.Load(),
		Total:      dl.total,
		StartedAt:  dl.startedAt.UnixMilli(),
		FinishedAt: time.Now().UnixMilli(),
		Status:     "ok",
	}
	if err != nil {
		rec.Status = "failed"
		rec.Error = err.Error()
	}
	c.mu.Lock()
	c.history = append([]DownloadStatus{rec}, c.history...)
	if len(c.history) > historyCap {
		c.history = c.history[:historyCap]
	}
	c.mu.Unlock()
}

// Lookup 返回已缓存文件路径；未命中返回空串。
func (c *Cache) Lookup(ctx context.Context, ref provider.TrackRef) string {
	if row, err := c.st.GetCacheRow(ctx, ref.String()); err == nil {
		if _, statErr := os.Stat(row.FilePath); statErr == nil {
			c.st.TouchCacheRow(ctx, ref.String())
			return row.FilePath
		}
		c.st.DeleteCacheRow(ctx, ref.String())
	}
	return ""
}

// resolveFile 对 file:// 定位符（local provider）短路：原文件即缓存。
func (c *Cache) resolveFile(ctx context.Context, ref provider.TrackRef) (string, bool, error) {
	p, _, err := c.reg.ForRef(ref)
	if err != nil {
		return "", false, err
	}
	loc, err := p.Resolve(ctx, ref)
	if err != nil {
		return "", false, err
	}
	if loc.IsFile() {
		return loc.FilePath(), true, nil
	}
	return "", false, nil
}

// Open 返回曲目对应的本地文件，未缓存则同步拉取完成。
// 供预取和需要 Range 的完整文件服务使用。
func (c *Cache) Open(ctx context.Context, ref provider.TrackRef) (*os.File, error) {
	if path := c.Lookup(ctx, ref); path != "" {
		return os.Open(path)
	}
	if path, ok, err := c.resolveFile(ctx, ref); err != nil {
		return nil, err
	} else if ok {
		return os.Open(path)
	}
	if err := c.ensure(ctx, ref); err != nil {
		return nil, err
	}
	path := c.Lookup(ctx, ref)
	if path == "" {
		return nil, ErrNotFound
	}
	return os.Open(path)
}

// OpenStream 面向 /stream 的顺序读取：缓存命中直接开文件；
// 未命中且为首个请求者时，返回边拉边写的 tee 流——
// 客户端首字节延迟 ≈ 源站响应延迟，同时后台落盘成缓存。
// 跟随者阻塞等待首个请求完成，然后读完整缓存文件。
//
// 错误重试：leader 异常（如客户端探测后断开）不应传染给
// 同时等待的 follower——跟随者收到错误后轮替为新 leader 重试。
func (c *Cache) OpenStream(ctx context.Context, ref provider.TrackRef) (io.ReadCloser, error) {
	var lastErr error
	for attempt := range 3 {
		rc, retry, err := c.openStreamOnce(ctx, ref)
		if err == nil {
			return rc, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
		log.Printf("[cache] %s: %v (retry %d)", ref, err, attempt+1)
	}
	return nil, fmt.Errorf("cache: %s: %w (after retries)", ref, lastErr)
}

func (c *Cache) openStreamOnce(ctx context.Context, ref provider.TrackRef) (io.ReadCloser, bool, error) {
	if path := c.Lookup(ctx, ref); path != "" {
		f, err := os.Open(path)
		return f, false, err
	}
	if path, ok, err := c.resolveFile(ctx, ref); err != nil {
		return nil, false, err
	} else if ok {
		f, err := os.Open(path)
		return f, false, err
	}

	p, _, err := c.reg.ForRef(ref)
	if err != nil {
		return nil, false, err
	}
	loc, err := p.Resolve(ctx, ref)
	if err != nil {
		return nil, false, err
	}

	c.mu.Lock()
	if dl, ok := c.inflight[ref]; ok {
		c.mu.Unlock()
		select {
		case <-dl.done:
			if dl.err != nil {
				// leader 失败可轮替重试
				return nil, true, dl.err
			}
			path := c.Lookup(ctx, ref)
			if path == "" {
				return nil, true, ErrNotFound
			}
			f, err := os.Open(path)
			return f, false, err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	dl := &download{done: make(chan struct{}), startedAt: time.Now(), total: -1}
	c.inflight[ref] = dl
	c.mu.Unlock()

	// 发起上游请求。注意：不用调用方 ctx——下载的生命周期独立于
	// 首个客户端连接（客户端断开后转后台继续，见 teeReader.drain）。
	upCtx, upCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	req, err := http.NewRequestWithContext(upCtx, http.MethodGet, loc.URL, nil)
	if err != nil {
		upCancel()
		c.finishInflight(ref, dl, err)
		return nil, true, err
	}
	for k, vs := range loc.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		upCancel()
		c.finishInflight(ref, dl, fmt.Errorf("fetch %s: %w", ref, err))
		return nil, true, fmt.Errorf("fetch: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		upCancel()
		err := fmt.Errorf("upstream status %d", resp.StatusCode)
		c.finishInflight(ref, dl, fmt.Errorf("fetch %s: %w", ref, err))
		return nil, false, err // 上游明确拒绝，重试无意义
	}
	dl.total = resp.ContentLength // 可能为 -1（未知）
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		resp.Body.Close()
		upCancel()
		c.finishInflight(ref, dl, err)
		return nil, false, err
	}
	tmp, err := os.CreateTemp(c.dir, "dl-*")
	if err != nil {
		resp.Body.Close()
		upCancel()
		c.finishInflight(ref, dl, err)
		return nil, false, err
	}

	log.Printf("[cache] %s: download started (%s)", ref, loc.URL)
	return &teeReader{
		c: c, ref: ref, dl: dl, loc: loc,
		body: resp.Body, tmp: tmp, cancel: upCancel,
	}, false, nil
}

// ensure 阻塞直到曲目完成缓存（预取路径）。
func (c *Cache) ensure(ctx context.Context, ref provider.TrackRef) error {
	rc, err := c.OpenStream(ctx, ref)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func (c *Cache) finishInflight(ref provider.TrackRef, dl *download, err error) {
	dl.err = err
	c.recordHistory(ref, dl, err)
	c.mu.Lock()
	delete(c.inflight, ref)
	c.mu.Unlock()
	close(dl.done)
}

// teeReader 读上游的同时写临时文件。
// 生命周期与首个客户端连接解耦：客户端提前断开时，下载移交后台
// goroutine 继续完成（drain），缓存照常客成——
// 这是抗 MPV"探测-断开-重开"模式的关键。
type teeReader struct {
	c      *Cache
	ref    provider.TrackRef
	dl     *download
	loc    provider.StreamLocator
	body   io.ReadCloser
	tmp    *os.File
	cancel context.CancelFunc // 上游请求的超时控制

	mu        sync.Mutex
	done      bool // 已读完并 finalize
	handedOff bool // 已移交后台 drain
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.body.Read(p)
	if n > 0 {
		wn, werr := t.tmp.Write(p[:n])
		t.dl.fetched.Add(int64(wn))
		if werr != nil && err == nil {
			err = werr
		}
	}
	if err == io.EOF {
		t.finalize()
	}
	return n, err
}

// Close 由 HTTP 层defer调用。客户端提前断开（未读到 EOF）时，
// 不中止下载——移交后台继续，缓存照常客成，后续请求直接命中。
func (t *teeReader) Close() error {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return t.body.Close()
	}
	if t.handedOff {
		t.mu.Unlock()
		return nil
	}
	t.handedOff = true
	t.mu.Unlock()
	log.Printf("[cache] %s: client disconnected, download continues in background", t.ref)
	go t.drain()
	return nil
}

// drain 后台完成剩余下载。
func (t *teeReader) drain() {
	n, err := io.Copy(t.tmp, t.body)
	t.body.Close()
	t.dl.fetched.Add(n)
	if err != nil {
		t.tmp.Close()
		os.Remove(t.tmp.Name())
		t.cancel()
		log.Printf("[cache] %s: background download failed: %v", t.ref, err)
		t.c.finishInflight(t.ref, t.dl, fmt.Errorf("background drain: %w", err))
		return
	}
	t.finalize()
}

func (t *teeReader) finalize() {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	t.mu.Unlock()
	defer t.cancel()
	size := t.dl.fetched.Load()

	tmpPath := t.tmp.Name()
	if err := t.tmp.Close(); err != nil {
		os.Remove(tmpPath)
		t.c.finishInflight(t.ref, t.dl, err)
		return
	}
	ext := ".bin"
	if t.loc.Format != "" {
		ext = "." + t.loc.Format
	}
	final := filepath.Join(t.c.dir, sanitize(string(t.ref))+ext)
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		t.c.finishInflight(t.ref, t.dl, err)
		return
	}
	now := time.Now().UnixMilli()
	err := t.c.st.PutCacheRow(context.Background(), store.CacheRow{
		TrackRef: t.ref.String(), FilePath: final, SizeBytes: size,
		BitrateKbps:    t.loc.BitrateKbps,
		LastAccessedAt: now, CreatedAt: now,
	})
	t.c.finishInflight(t.ref, t.dl, err)
	if err == nil {
		log.Printf("[cache] %s: cached %d bytes -> %s", t.ref, size, final)
		go t.c.evict()
	}
}

// Prefetch 后台预拉取（用于队列预解析）。错误静默——播放时还会重试。
func (c *Cache) Prefetch(ref provider.TrackRef) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c.ensure(ctx, ref)
}

// evict 超出容量时按 LRU 清理。
func (c *Cache) evict() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := c.st.ListCacheRows(ctx) // 按 last_accessed_at 升序
	if err != nil {
		return
	}
	var total int64
	for _, r := range rows {
		total += r.SizeBytes
	}
	for _, r := range rows {
		if total <= c.maxBytes {
			break
		}
		if err := os.Remove(r.FilePath); err == nil {
			c.st.DeleteCacheRow(ctx, r.TrackRef)
			total -= r.SizeBytes
		}
	}
}

// EvictTrack 手动清理单条（管理接口用）。
func (c *Cache) EvictTrack(ctx context.Context, ref provider.TrackRef) error {
	row, err := c.st.GetCacheRow(ctx, ref.String())
	if err != nil {
		return ErrNotFound
	}
	os.Remove(row.FilePath)
	return c.st.DeleteCacheRow(ctx, ref.String())
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
