package cache

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

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
	return New(dir, 1024, st, provider.NewRegistry()), st, dir
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
