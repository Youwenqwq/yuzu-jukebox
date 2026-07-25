// Package cache 实现流式缓存：首次拉流时边下边服务（tee），
// 后续请求直接命中本地文件。凭据只在 Resolve/拉流的服务端路径上使用。
package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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
}

// download 表示一次进行中的拉取；跟随者等待完成后读缓存文件。
type download struct {
	done chan struct{}
	err  error
}

func New(dir string, maxBytes int64, st *store.Store, reg *provider.Registry) *Cache {
	return &Cache{
		dir:      dir,
		maxBytes: maxBytes,
		st:       st,
		reg:      reg,
		client:   &http.Client{Timeout: 0}, // 流式拉取，不设总超时；由 ctx 控制
		inflight: map[provider.TrackRef]*download{},
	}
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
func (c *Cache) OpenStream(ctx context.Context, ref provider.TrackRef) (io.ReadCloser, error) {
	if path := c.Lookup(ctx, ref); path != "" {
		return os.Open(path)
	}
	if path, ok, err := c.resolveFile(ctx, ref); err != nil {
		return nil, err
	} else if ok {
		return os.Open(path)
	}

	p, _, err := c.reg.ForRef(ref)
	if err != nil {
		return nil, err
	}
	loc, err := p.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if dl, ok := c.inflight[ref]; ok {
		c.mu.Unlock()
		select {
		case <-dl.done:
			if dl.err != nil {
				return nil, dl.err
			}
			path := c.Lookup(ctx, ref)
			if path == "" {
				return nil, ErrNotFound
			}
			return os.Open(path)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	dl := &download{done: make(chan struct{})}
	c.inflight[ref] = dl
	c.mu.Unlock()

	// 发起上游请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.URL, nil)
	if err != nil {
		c.finishInflight(ref, dl, err)
		return nil, err
	}
	for k, vs := range loc.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.finishInflight(ref, dl, fmt.Errorf("fetch %s: %w", ref, err))
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		err := fmt.Errorf("fetch %s: upstream status %d", ref, resp.StatusCode)
		c.finishInflight(ref, dl, err)
		return nil, err
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		resp.Body.Close()
		c.finishInflight(ref, dl, err)
		return nil, err
	}
	tmp, err := os.CreateTemp(c.dir, "dl-*")
	if err != nil {
		resp.Body.Close()
		c.finishInflight(ref, dl, err)
		return nil, err
	}

	return &teeReader{
		ctx:  ctx,
		c:    c,
		ref:  ref,
		dl:   dl,
		loc:  loc,
		body: resp.Body,
		tmp:  tmp,
	}, nil
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
	c.mu.Lock()
	delete(c.inflight, ref)
	c.mu.Unlock()
	close(dl.done)
}

// teeReader 读上游的同时写临时文件；读到 EOF 时原子改名并登记缓存。
type teeReader struct {
	ctx  context.Context
	c    *Cache
	ref  provider.TrackRef
	dl   *download
	loc  provider.StreamLocator
	body io.ReadCloser
	tmp  *os.File
	size int64
	done bool
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.body.Read(p)
	if n > 0 {
		wn, werr := t.tmp.Write(p[:n])
		t.size += int64(wn)
		if werr != nil && err == nil {
			err = werr
		}
	}
	if err == io.EOF {
		t.finalize()
	}
	return n, err
}

func (t *teeReader) Close() error {
	t.body.Close()
	if !t.done {
		// 客户端提前断开：放弃缓存，清理现场
		t.tmp.Close()
		os.Remove(t.tmp.Name())
		t.done = true
		t.c.finishInflight(t.ref, t.dl, errors.New("download aborted: client disconnected"))
	}
	return nil
}

func (t *teeReader) finalize() {
	if t.done {
		return
	}
	t.done = true
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
	err := t.c.st.PutCacheRow(t.ctx, store.CacheRow{
		TrackRef: t.ref.String(), FilePath: final, SizeBytes: t.size,
		LastAccessedAt: now, CreatedAt: now,
	})
	t.c.finishInflight(t.ref, t.dl, err)
	if err == nil {
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
