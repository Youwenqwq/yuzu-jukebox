package cache

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

type objectLimitProvider struct {
	baseURL string
}

func (p *objectLimitProvider) ID() string { return "limit" }

func (p *objectLimitProvider) Search(context.Context, string, int, int) ([]provider.Track, error) {
	return nil, nil
}

func (p *objectLimitProvider) GetTrack(context.Context, provider.TrackRef) (provider.Track, error) {
	return provider.Track{}, nil
}

func (p *objectLimitProvider) Resolve(_ context.Context, ref provider.TrackRef) (provider.StreamLocator, error) {
	_, id, _ := ref.Split()
	return provider.StreamLocator{URL: p.baseURL + "/" + id, Format: "mp3"}, nil
}

func setupPruneCache(t *testing.T) (*Cache, *store.Store, string) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := filepath.Join(root, "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return New(dir, 1024, 0, st, provider.NewRegistry()), st, dir
}

func putPruneRow(t *testing.T, st *store.Store, dir, ref string, size int64, lastAccessed time.Time) string {
	t.Helper()
	path := filepath.Join(dir, ref+".bin")
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	at := lastAccessed.UnixMilli()
	if err := st.PutCacheRow(context.Background(), store.CacheRow{
		TrackRef: ref, FilePath: path, SizeBytes: size,
		LastAccessedAt: at, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPruneUnusedSelectsByAgeAndReportsFreedBytes(t *testing.T) {
	c, st, dir := setupPruneCache(t)
	now := time.Now()
	oldPath := putPruneRow(t, st, dir, "old", 3, now.Add(-72*time.Hour))
	recentPath := putPruneRow(t, st, dir, "recent", 5, now.Add(-12*time.Hour))

	if got := c.TotalBytes(); got != 8 {
		t.Fatalf("TotalBytes before prune = %d, want 8", got)
	}
	evicted, freed, err := c.PruneUnused(context.Background(), 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 1 || freed != 3 {
		t.Fatalf("PruneUnused = (%d, %d), want (1, 3)", evicted, freed)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old cache file still present: %v", err)
	}
	if _, err := st.GetCacheRow(context.Background(), "old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old cache row lookup = %v, want sql.ErrNoRows", err)
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatalf("recent cache file removed: %v", err)
	}
	if got := c.TotalBytes(); got != 5 {
		t.Fatalf("TotalBytes after prune = %d, want 5", got)
	}
}

func TestPruneUnusedZeroEvictsAllExceptInflight(t *testing.T) {
	c, st, dir := setupPruneCache(t)
	now := time.Now()
	idlePath := putPruneRow(t, st, dir, "idle", 4, now)
	activePath := putPruneRow(t, st, dir, "active", 6, now)

	c.mu.Lock()
	c.inflight[provider.TrackRef("active")] = &download{}
	c.mu.Unlock()

	evicted, freed, err := c.PruneUnused(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 1 || freed != 4 {
		t.Fatalf("PruneUnused(0) = (%d, %d), want (1, 4)", evicted, freed)
	}
	if _, err := os.Stat(idlePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("idle cache file still present: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("inflight cache file removed: %v", err)
	}
	if _, err := st.GetCacheRow(context.Background(), "active"); err != nil {
		t.Fatalf("inflight cache row removed: %v", err)
	}
}

func TestObjectDownloadLimitRejectsDeclaredAndStreamingSizes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/declared":
			w.Header().Set("Content-Length", "6")
			_, _ = io.WriteString(w, "123456")
		case "/chunked":
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "123456")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg := provider.NewRegistry()
	reg.Register(&objectLimitProvider{baseURL: upstream.URL})
	dir := filepath.Join(root, "cache")
	c := New(dir, 1024, 5, st, reg)

	if _, err := c.OpenStream(context.Background(), provider.TrackRef("limit:declared")); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("declared-size OpenStream error = %v", err)
	}

	rc, err := c.OpenStream(context.Background(), provider.TrackRef("limit:chunked"))
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if !errors.Is(readErr, ErrObjectTooLarge) {
		t.Fatalf("streaming read error = %v", readErr)
	}
	if string(data) != "12345" {
		t.Fatalf("streaming data = %q, want capped prefix", data)
	}
	if path := c.Lookup(context.Background(), provider.TrackRef("limit:chunked")); path != "" {
		t.Fatalf("oversized object was cached at %s", path)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "dl-*")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary downloads after rejection = %v, err %v", matches, err)
	}
}

type cacheTestTransport func(*http.Request) (*http.Response, error)

func (f cacheTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestTruncatedStreamDoesNotPoisonCache(t *testing.T) {
	const complete = "0123456789"
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, complete[:5])
			return
		}
		_, _ = io.WriteString(w, complete)
	}))
	t.Cleanup(upstream.Close)
	c, _, _ := setupPruneCache(t)
	c.reg.Register(&objectLimitProvider{baseURL: upstream.URL})
	// A reader may report an error once and EOF on the next read. Close must
	// not turn that failure into a successful background-drain finalization.
	c.client.Transport = cacheTestTransport(func(r *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(r)
		if err == nil && requests.Load() == 1 {
			response.Body = struct {
				io.Reader
				io.Closer
			}{
				Reader: iotest.TimeoutReader(io.LimitReader(response.Body, 5)),
				Closer: response.Body,
			}
		}
		return response, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref := provider.TrackRef("limit:truncated")
	rc, err := c.OpenStream(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.inflight[ref].done
	c.mu.Unlock()
	_, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if !errors.Is(readErr, iotest.ErrTimeout) {
		t.Fatalf("truncated read error = %v", readErr)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	f, err := c.Open(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != complete {
		t.Fatalf("reopened track = %q, err=%v; incomplete download must not become a cache hit", data, err)
	}
}

func TestStreamProbeDisconnectCompletesCache(t *testing.T) {
	const complete = "0123456789"
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", "10")
		_, _ = io.WriteString(w, complete)
	}))
	t.Cleanup(upstream.Close)
	c, _, _ := setupPruneCache(t)
	c.reg.Register(&objectLimitProvider{baseURL: upstream.URL})
	ctx, cancel := context.WithCancel(context.Background())
	ref := provider.TrackRef("limit:probe")
	rc, err := c.OpenStream(ctx, ref)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	prefix := make([]byte, 2)
	if _, err := io.ReadFull(rc, prefix); err != nil {
		cancel()
		_ = rc.Close()
		t.Fatal(err)
	}
	cancel()
	_ = rc.Close()
	followerCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	f, err := c.Open(followerCtx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != complete {
		t.Fatalf("reopened probe = %q, err=%v", data, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("probe disconnect caused another source download: %d", requests.Load())
	}
}

func TestStreamTimeoutMeasuresUpstreamReadNotPlayerPause(t *testing.T) {
	for _, playerPause := range []bool{true, false} {
		name := "stalled upstream"
		if playerPause {
			name = "paused player"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "10")
				_, _ = io.WriteString(w, "01")
				w.(http.Flusher).Flush()
				select {
				case <-release:
					_, _ = io.WriteString(w, "23456789")
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(upstream.Close)
			c, _, _ := setupPruneCache(t)
			c.readTimeout = 500 * time.Millisecond
			c.reg.Register(&objectLimitProvider{baseURL: upstream.URL})
			ref := provider.TrackRef("limit:slow")
			rc, err := c.OpenStream(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			prefix := make([]byte, 2)
			if _, err := io.ReadFull(rc, prefix); err != nil {
				t.Fatal(err)
			}
			if playerPause {
				// No upstream Read is pending while the player is paused.
				time.Sleep(2 * c.readTimeout)
				close(release)
				released = true
			}
			rest, err := io.ReadAll(rc)
			if playerPause {
				if err != nil || string(rest) != "23456789" {
					t.Fatalf("resumed stream = %q, err=%v", rest, err)
				}
			} else {
				if err == nil {
					t.Fatal("stalled upstream was accepted as a complete download")
				}
				if path := c.Lookup(context.Background(), ref); path != "" {
					t.Fatalf("timed-out stream was cached at %s", path)
				}
			}
		})
	}
}
