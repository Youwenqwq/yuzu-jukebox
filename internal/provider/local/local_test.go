package local

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestAudioContainerAllowlist(t *testing.T) {
	for _, format := range []string{"mp3", "wav", "mov,mp4,m4a,3gp,3g2,mj2", "aac", "flac", "ogg", "opus", "ape", "asf", "aiff"} {
		if !isAllowedAudioContainer(format) {
			t.Errorf("audio container %q was rejected", format)
		}
	}
	for _, format := range []string{"", "avi", "image2", "data"} {
		if isAllowedAudioContainer(format) {
			t.Errorf("non-audio container %q was accepted", format)
		}
	}
}

func TestWAVFmtChunkSizeGuards(t *testing.T) {
	writeFixture := func(name string, declaredSize uint32) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		data := make([]byte, 20)
		copy(data[0:4], "RIFF")
		copy(data[8:12], "WAVE")
		copy(data[12:16], "fmt ")
		binary.LittleEndian.PutUint32(data[16:20], declaredSize)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	if _, err := wavDurationMs(writeFixture("oversized.wav", 64<<20+1)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized fmt chunk error = %v", err)
	}
	if _, err := wavDurationMs(writeFixture("past-eof.wav", 16)); err == nil || !strings.Contains(err.Error(), "exceeds file size") {
		t.Fatalf("file-bound fmt chunk error = %v", err)
	}
}
