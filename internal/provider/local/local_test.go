package local

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestSearchPaginates(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, file := range []store.MediaFile{
		{ID: "old", Filename: "old.mp3", Title: "Match Old", CreatedAt: 10},
		{ID: "middle", Filename: "middle.mp3", Title: "Match Middle", CreatedAt: 20},
		{ID: "new", Filename: "new.mp3", Title: "Match New", CreatedAt: 30},
	} {
		if err := st.AddMediaFile(ctx, file); err != nil {
			t.Fatal(err)
		}
	}

	got, err := New("", st).Search(ctx, "Match", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ref != provider.NewRef("local", "middle") {
		t.Fatalf("Search() = %#v, want local:middle", got)
	}
}

func TestDeleteKeepsRowDeletedWhenFileRemovalFails(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mediaDir := filepath.Join(root, "media")
	blockedPath := filepath.Join(mediaDir, "blocked.mp3")
	if err := os.MkdirAll(blockedPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blockedPath, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.AddMediaFile(ctx, store.MediaFile{ID: "blocked", Filename: "blocked.mp3", Title: "Blocked"}); err != nil {
		t.Fatal(err)
	}

	p := New(mediaDir, st)
	if err := p.Delete(ctx, provider.TrackRef("local:blocked")); err != nil {
		t.Fatalf("Delete returned file removal error: %v", err)
	}
	if _, err := st.GetMediaFile(ctx, "blocked"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("media row after delete: %v, want sql.ErrNoRows", err)
	}
	if _, err := os.Stat(blockedPath); err != nil {
		t.Fatalf("failed-removal fixture unexpectedly disappeared: %v", err)
	}
}
