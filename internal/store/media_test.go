package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestListAndDeleteMediaFiles(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	files := []MediaFile{
		{ID: "old", Filename: "old.mp3", Title: "Old", CreatedAt: 10},
		{ID: "new", Filename: "new.mp3", Title: "New", CreatedAt: 30},
		{ID: "middle", Filename: "middle.mp3", Title: "Middle", CreatedAt: 20},
	}
	for _, file := range files {
		if err := st.AddMediaFile(ctx, file); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.ListMediaFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new", "middle", "old"}
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("position %d: got %q, want %q", i, got[i].ID, id)
		}
	}

	if err := st.DeleteMediaFile(ctx, "middle"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMediaFile(ctx, "middle"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetMediaFile after delete: %v, want sql.ErrNoRows", err)
	}
	if err := st.DeleteMediaFile(ctx, "middle"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second delete: %v, want sql.ErrNoRows", err)
	}
}

func TestSearchMediaFilesPaging(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	for _, file := range []MediaFile{
		{ID: "old", Filename: "old.mp3", Title: "Song Old", CreatedAt: 10},
		{ID: "middle", Filename: "middle.mp3", Title: "Song Middle", CreatedAt: 20},
		{ID: "new", Filename: "new.mp3", Title: "Song New", CreatedAt: 30},
		{ID: "other", Filename: "other.mp3", Title: "Other", CreatedAt: 40},
	} {
		if err := st.AddMediaFile(ctx, file); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.SearchMediaFiles(ctx, "Song", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "middle" || got[1].ID != "old" {
		t.Fatalf("SearchMediaFiles() = %#v, want middle then old", got)
	}
}

func TestListMediaFilesEmptyIsNonNil(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	got, err := st.ListMediaFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want non-nil empty slice", got)
	}
}
