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

var (
	ErrNotFound       = errors.New("track not cached")
	ErrObjectTooLarge = errors.New("cache object exceeds max_object_bytes")
)

type Cache struct {
	dir            string
	maxBytes       int64
	maxObjectBytes int64
	st             *store.Store
	reg            *provider.Registry

	client      *http.Client
	readTimeout time.Duration

	mu       sync.Mutex
	inflight map[provider.TrackRef]*download
	history  []DownloadStatus // 最近完成的下载（最新在前，环形上限 historyCap）
}

// download 表示一次进行中的拉取；跟随者独立读取仍在增长的文件。
type download struct {
	done      chan struct{}
	ready     chan struct{}
	opened    chan struct{}
	readyOnce sync.Once
	openOnce  sync.Once
	mu        sync.Mutex // 保护路径、终态及前沿通知
	path      string
	progress  chan struct{}
	finished  bool
	err       error
	startedAt time.Time
	total     int64 // 上游 Content-Length；-1 = 未知
	fetched   atomic.Int64
}

// ReadyMinBytes 是开播前需缓冲的最小字节数（容器头/索引可读）。
const ReadyMinBytes = 128 << 10

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

func New(dir string, maxBytes, maxObjectBytes int64, st *store.Store, reg *provider.Registry) *Cache {
	// 清理上次进程退出时遗留的临时下载文件
	if matches, _ := filepath.Glob(filepath.Join(dir, "dl-*")); len(matches) > 0 {
		for _, m := range matches {
			os.Remove(m)
		}
		log.Printf("[cache] removed %d stale temp files", len(matches))
	}
	return &Cache{
		dir:            dir,
		maxBytes:       maxBytes,
		maxObjectBytes: maxObjectBytes,
		st:             st,
		reg:            reg,
		client:         &http.Client{Timeout: 0},
		readTimeout:    30 * time.Second,
		inflight:       map[provider.TrackRef]*download{},
	}
}

// MaxBytes 返回当前缓存容量上限。
func (c *Cache) MaxBytes() int64 {
	return c.maxBytes
}

// TotalBytes 返回全部缓存索引条目记录的字节数总和。
func (c *Cache) TotalBytes() int64 {
	rows, err := c.st.ListCacheRows(context.Background())
	if err != nil {
		return 0
	}
	var total int64
	for _, row := range rows {
		total += row.SizeBytes
	}
	return total
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
// 跟随者独立读取增长文件，不必等待整曲完成。
// 建流失败可重试；读取期间的错误直接返回给调用方。
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
	path, dl, leader := c.downloadFor(ref)
	if path != "" {
		f, err := os.Open(path)
		return f, false, err
	}
	if !leader {
		rc, _, err := c.openDownload(ctx, dl, 0)
		return rc, err != nil && ctx.Err() == nil && !errors.Is(err, ErrObjectTooLarge), err
	}
	return c.startStream(ctx, ref, dl)
}

// downloadFor 将缓存复查与在途登记放在同一临界区，避免完成瞬间重复下载。
func (c *Cache) downloadFor(ref provider.TrackRef) (string, *download, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dl := c.inflight[ref]; dl != nil {
		return "", dl, false
	}
	if path := c.Lookup(context.Background(), ref); path != "" {
		return path, nil, false
	}
	dl := &download{
		done: make(chan struct{}), ready: make(chan struct{}),
		opened: make(chan struct{}), progress: make(chan struct{}),
		startedAt: time.Now(), total: -1,
	}
	c.inflight[ref] = dl
	return "", dl, true
}

// backgroundDownload 的首个请求者立即移交后台；调用方只拥有等待预算。
func (c *Cache) backgroundDownload(ref provider.TrackRef) (string, *download) {
	path, dl, leader := c.downloadFor(ref)
	if leader {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), c.readTimeout)
			defer cancel()
			rc, _, err := c.startStream(ctx, ref, dl)
			if err == nil {
				// teeReader.Close 会立即启动 drain；本地文件则直接关闭。
				_ = rc.Close()
			}
		}()
	}
	return path, dl
}

func (c *Cache) startStream(ctx context.Context, ref provider.TrackRef, dl *download) (io.ReadCloser, bool, error) {
	p, _, err := c.reg.ForRef(ref)
	if err != nil {
		c.finishInflight(ref, dl, err)
		return nil, false, err
	}
	loc, err := p.Resolve(ctx, ref)
	if err != nil {
		c.finishInflight(ref, dl, err)
		return nil, false, err
	}
	if loc.IsFile() {
		f, err := os.Open(loc.FilePath())
		if err == nil {
			dl.mu.Lock()
			dl.path = loc.FilePath()
			dl.mu.Unlock()
		}
		c.finishInflight(ref, dl, err)
		return f, false, err
	}

	// 发起上游请求。注意：不用调用方 ctx——下载的生命周期独立于
	// 首个客户端连接（客户端断开后转后台继续，见 teeReader.drain）。
	upCtx, cancel := context.WithCancelCause(context.Background())
	timer := time.AfterFunc(c.readTimeout, func() { cancel(context.DeadlineExceeded) })
	upCancel := func() {
		timer.Stop()
		cancel(context.Canceled)
	}
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
	timer.Stop()
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
	resp.Body = &readTimeoutBody{ReadCloser: resp.Body, timer: timer, timeout: c.readTimeout}
	c.mu.Lock()
	dl.total = resp.ContentLength // 可能为 -1（未知）
	c.mu.Unlock()
	if c.maxObjectBytes > 0 && resp.ContentLength > c.maxObjectBytes {
		resp.Body.Close()
		upCancel()
		err := fmt.Errorf("%w: declared size %d exceeds limit %d", ErrObjectTooLarge, resp.ContentLength, c.maxObjectBytes)
		c.finishInflight(ref, dl, err)
		return nil, false, err
	}
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
	dl.mu.Lock()
	dl.path = tmp.Name()
	dl.openOnce.Do(func() { close(dl.opened) })
	dl.mu.Unlock()

	log.Printf("[cache] %s: download started (%s)", ref, loc.URL)
	return &teeReader{
		c: c, ref: ref, dl: dl, loc: loc,
		body: resp.Body, tmp: tmp, cancel: upCancel,
	}, false, nil
}

// WaitReady 等待首段缓冲或完整落盘；调用方取消不会中断后台下载。
func (c *Cache) WaitReady(ctx context.Context, ref provider.TrackRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, dl := c.backgroundDownload(ref)
	if path != "" {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-dl.ready:
		dl.mu.Lock()
		defer dl.mu.Unlock()
		return dl.err
	}
}

// OpenLive 从 offset 起读取完整缓存或增长文件；到达前沿时等待下载推进。
func (c *Cache) OpenLive(ctx context.Context, ref provider.TrackRef, offset int64) (rc io.ReadCloser, total int64, err error) {
	if err := ctx.Err(); err != nil {
		return nil, -1, err
	}
	if offset < 0 {
		return nil, -1, fmt.Errorf("cache: negative offset %d", offset)
	}
	path, dl := c.backgroundDownload(ref)
	if path != "" {
		return openFileAt(path, offset)
	}
	return c.openDownload(ctx, dl, offset)
}

func openFileAt(path string, offset int64) (io.ReadCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, -1, err
	}
	info, err := f.Stat()
	if err == nil {
		_, err = f.Seek(offset, io.SeekStart)
	}
	if err != nil {
		f.Close()
		return nil, -1, err
	}
	return f, info.Size(), nil
}

func (c *Cache) openDownload(ctx context.Context, dl *download, offset int64) (io.ReadCloser, int64, error) {
	select {
	case <-ctx.Done():
		return nil, -1, ctx.Err()
	case <-dl.opened:
	}
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.err != nil {
		return nil, -1, dl.err
	}
	// 与 rename 共用锁，避免路径由临时文件切换为完整缓存时开错文件。
	rc, size, err := openFileAt(dl.path, offset)
	if err != nil {
		return nil, -1, err
	}
	if dl.finished {
		return rc, size, nil
	}
	readerCtx, cancel := context.WithCancel(ctx)
	return &liveReader{file: rc, dl: dl, ctx: readerCtx, cancel: cancel}, dl.total, nil
}

type liveReader struct {
	file   io.ReadCloser
	dl     *download
	ctx    context.Context
	cancel context.CancelFunc
}

func (r *liveReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		r.dl.mu.Lock()
		progress, finished, downloadErr := r.dl.progress, r.dl.finished, r.dl.err
		r.dl.mu.Unlock()
		n, err := r.file.Read(p)
		if n > 0 || err != io.EOF {
			return n, err
		}
		if finished {
			if downloadErr != nil {
				return 0, downloadErr
			}
			return 0, io.EOF
		}
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-progress:
		}
	}
}

func (r *liveReader) Close() error {
	r.cancel()
	return r.file.Close()
}

// ensure 保留整曲阻塞语义，但等待取消不会取消下载。
func (c *Cache) ensure(ctx context.Context, ref provider.TrackRef) error {
	path, dl := c.backgroundDownload(ref)
	if path != "" {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-dl.done:
		return dl.err
	}
}

func (c *Cache) finishInflight(ref provider.TrackRef, dl *download, err error) {
	c.recordHistory(ref, dl, err)
	c.mu.Lock()
	dl.mu.Lock()
	dl.err = err
	dl.finished = true
	dl.openOnce.Do(func() { close(dl.opened) })
	dl.readyOnce.Do(func() { close(dl.ready) })
	close(dl.progress)
	close(dl.done)
	delete(c.inflight, ref)
	dl.mu.Unlock()
	c.mu.Unlock()
}

// advance 在每次写入后唤醒前沿读者，首段达标后只发一次就绪信号。
func (dl *download) advance(n int) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.fetched.Add(int64(n)) >= ReadyMinBytes {
		dl.readyOnce.Do(func() { close(dl.ready) })
	}
	close(dl.progress)
	dl.progress = make(chan struct{})
}

// readTimeoutBody limits stalled upstream reads, not the lifetime of a track.
// Time spent waiting for the player to request more bytes must not expire a
// healthy download. The same reader is used after handing off to background drain.
type readTimeoutBody struct {
	io.ReadCloser
	timer   *time.Timer
	timeout time.Duration
}

func (b *readTimeoutBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.timeout)
	n, err := b.ReadCloser.Read(p)
	b.timer.Stop()
	return n, err
}

func (b *readTimeoutBody) Close() error {
	b.timer.Stop()
	return b.ReadCloser.Close()
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
	if len(p) == 0 {
		return 0, nil
	}
	remaining := int64(len(p))
	if t.c.maxObjectBytes > 0 {
		remaining = t.c.maxObjectBytes - t.dl.fetched.Load()
		if remaining < 0 {
			err := fmt.Errorf("%w: limit %d", ErrObjectTooLarge, t.c.maxObjectBytes)
			t.fail(err)
			return 0, err
		}
		if int64(len(p)) > remaining {
			p = p[:remaining+1]
		}
	}
	n, err := t.body.Read(p)
	if t.c.maxObjectBytes > 0 && int64(n) > remaining {
		allowed := int(remaining)
		if allowed > 0 {
			wn, writeErr := t.tmp.Write(p[:allowed])
			t.dl.advance(wn)
			if writeErr != nil {
				t.fail(writeErr)
				return wn, writeErr
			}
		}
		limitErr := fmt.Errorf("%w: limit %d", ErrObjectTooLarge, t.c.maxObjectBytes)
		t.fail(limitErr)
		return allowed, limitErr
	}
	if n > 0 {
		wn, writeErr := t.tmp.Write(p[:n])
		t.dl.advance(wn)
		if writeErr != nil {
			t.fail(writeErr)
			return wn, writeErr
		}
	}
	if err == io.EOF {
		if finalizeErr := t.finalize(); finalizeErr != nil {
			return n, finalizeErr
		}
	} else if err != nil {
		t.fail(err)
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
	// 复用 Read 的写入、阈值及前沿通知逻辑，不能直接 Copy 到临时文件。
	if _, err := io.Copy(io.Discard, t); err != nil {
		log.Printf("[cache] %s: background download failed: %v", t.ref, err)
	}
}

func (t *teeReader) fail(err error) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	t.mu.Unlock()
	t.body.Close()
	tmpPath := t.tmp.Name()
	t.tmp.Close()
	os.Remove(tmpPath)
	t.cancel()
	t.c.finishInflight(t.ref, t.dl, err)
}

func (t *teeReader) finalize() error {
	size := t.dl.fetched.Load()
	if t.c.maxObjectBytes > 0 && size > t.c.maxObjectBytes {
		err := fmt.Errorf("%w: limit %d", ErrObjectTooLarge, t.c.maxObjectBytes)
		t.fail(err)
		return err
	}
	if t.dl.total >= 0 && size != t.dl.total {
		err := fmt.Errorf("cache download size %d, expected %d: %w", size, t.dl.total, io.ErrUnexpectedEOF)
		t.fail(err)
		return err
	}
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return nil
	}
	t.done = true
	t.mu.Unlock()
	defer t.cancel()
	defer t.body.Close()

	tmpPath := t.tmp.Name()
	if err := t.tmp.Close(); err != nil {
		os.Remove(tmpPath)
		t.c.finishInflight(t.ref, t.dl, err)
		return err
	}
	ext := ".bin"
	if t.loc.Format != "" {
		ext = "." + t.loc.Format
	}
	final := filepath.Join(t.c.dir, sanitize(string(t.ref))+ext)
	t.dl.mu.Lock()
	renameErr := os.Rename(tmpPath, final)
	if renameErr == nil {
		t.dl.path = final
	}
	t.dl.mu.Unlock()
	if err := renameErr; err != nil {
		os.Remove(tmpPath)
		t.c.finishInflight(t.ref, t.dl, err)
		return err
	}
	now := time.Now().UnixMilli()
	err := t.c.st.PutCacheRow(context.Background(), store.CacheRow{
		TrackRef: t.ref.String(), FilePath: final, SizeBytes: size,
		BitrateKbps:    t.loc.BitrateKbps,
		LastAccessedAt: now, CreatedAt: now,
	})
	if err != nil {
		os.Remove(final)
	}
	t.c.finishInflight(t.ref, t.dl, err)
	if err == nil {
		log.Printf("[cache] %s: cached %d bytes -> %s", t.ref, size, final)
		go t.c.evict()
	}
	return err
}

// Prefetch 后台预拉取（用于队列预解析）。错误静默——播放时还会重试。
func (c *Cache) Prefetch(ref provider.TrackRef) {
	c.backgroundDownload(ref)
}

// evict 超出容量时按 LRU 清理。
func (c *Cache) evict() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c.mu.Lock()
	defer c.mu.Unlock()

	rows, err := c.st.ListCacheRows(ctx) // 按 last_accessed_at 升序
	if err != nil {
		return
	}
	var total int64
	for _, row := range rows {
		total += row.SizeBytes
	}
	for _, row := range rows {
		if total <= c.maxBytes {
			break
		}
		if _, downloading := c.inflight[provider.TrackRef(row.TrackRef)]; downloading {
			continue
		}
		if err := c.removeRow(ctx, row); err == nil {
			total -= row.SizeBytes
		}
	}
}

// PruneUnused 驱逐最后访问时间早于指定时长的缓存条目。
// olderThan 为 0 时驱逐全部条目；正在下载的条目始终跳过。
func (c *Cache) PruneUnused(ctx context.Context, olderThan time.Duration) (evicted int, freed int64, err error) {
	if olderThan < 0 {
		return 0, 0, errors.New("olderThan must not be negative")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	rows, err := c.st.ListCacheRows(ctx)
	if err != nil {
		return 0, 0, err
	}
	cutoff := time.Now().Add(-olderThan).UnixMilli()
	for _, row := range rows {
		if olderThan > 0 && row.LastAccessedAt >= cutoff {
			continue
		}
		if _, downloading := c.inflight[provider.TrackRef(row.TrackRef)]; downloading {
			continue
		}
		if err := c.removeRow(ctx, row); err != nil {
			return evicted, freed, err
		}
		evicted++
		freed += row.SizeBytes
	}
	return evicted, freed, nil
}

func (c *Cache) removeRow(ctx context.Context, row store.CacheRow) error {
	if err := os.Remove(row.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return c.st.DeleteCacheRow(ctx, row.TrackRef)
}

// EvictTrack 手动清理单条（管理接口用）。
func (c *Cache) EvictTrack(ctx context.Context, ref provider.TrackRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	row, err := c.st.GetCacheRow(ctx, ref.String())
	if err != nil {
		return ErrNotFound
	}
	return c.removeRow(ctx, row)
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
