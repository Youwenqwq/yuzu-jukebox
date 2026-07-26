package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func setupPlaylist(t *testing.T, n int) (*Store, string) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreatePlaylist(ctx, Playlist{ID: "pl_t", Name: "t", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	items := make([]PlaylistItem, n)
	for i := range items {
		items[i] = PlaylistItem{TrackRef: fmt.Sprintf("t%d", i+1), Title: fmt.Sprintf("t%d", i+1)}
	}
	if err := st.AppendPlaylistItems(ctx, "pl_t", items); err != nil {
		t.Fatal(err)
	}
	return st, "pl_t"
}

// orderOf 返回当前 track_ref 顺序，并断言序号为 1..len 连续。
func orderOf(t *testing.T, st *Store, plID string) []string {
	t.Helper()
	items, err := st.PlaylistItems(context.Background(), plID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]string, len(items))
	for i, it := range items {
		if it.Ord != i+1 {
			t.Fatalf("ord not contiguous: item %d has ord %d", i, it.Ord)
		}
		refs[i] = it.TrackRef
	}
	return refs
}

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}

func TestMovePlaylistItemToFront(t *testing.T) {
	st, pl := setupPlaylist(t, 5)
	final, err := st.MovePlaylistItem(context.Background(), pl, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if final != 1 {
		t.Fatalf("final ord %d, want 1", final)
	}
	assertOrder(t, orderOf(t, st, pl), "t4", "t1", "t2", "t3", "t5")
}

func TestMovePlaylistItemToMiddle(t *testing.T) {
	st, pl := setupPlaylist(t, 5)
	final, err := st.MovePlaylistItem(context.Background(), pl, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if final != 3 {
		t.Fatalf("final ord %d, want 3", final)
	}
	assertOrder(t, orderOf(t, st, pl), "t2", "t3", "t1", "t4", "t5")
}

func TestMovePlaylistItemToTail(t *testing.T) {
	st, pl := setupPlaylist(t, 5)
	final, err := st.MovePlaylistItem(context.Background(), pl, 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	if final != 5 {
		t.Fatalf("final ord %d, want 5", final)
	}
	assertOrder(t, orderOf(t, st, pl), "t1", "t3", "t4", "t5", "t2")
}

func TestMovePlaylistItemClamp(t *testing.T) {
	st, pl := setupPlaylist(t, 3)
	ctx := context.Background()

	// to_ord < 1 clamp 到 1
	final, err := st.MovePlaylistItem(ctx, pl, 3, -7)
	if err != nil {
		t.Fatal(err)
	}
	if final != 1 {
		t.Fatalf("final ord %d, want 1", final)
	}
	assertOrder(t, orderOf(t, st, pl), "t3", "t1", "t2")

	// to_ord > len clamp 到 len
	final, err = st.MovePlaylistItem(ctx, pl, 1, 99)
	if err != nil {
		t.Fatal(err)
	}
	if final != 3 {
		t.Fatalf("final ord %d, want 3", final)
	}
	assertOrder(t, orderOf(t, st, pl), "t1", "t2", "t3")
}

func TestMovePlaylistItemSamePosition(t *testing.T) {
	st, pl := setupPlaylist(t, 3)
	final, err := st.MovePlaylistItem(context.Background(), pl, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if final != 2 {
		t.Fatalf("final ord %d, want 2", final)
	}
	assertOrder(t, orderOf(t, st, pl), "t1", "t2", "t3")
}

func TestMovePlaylistItemOutOfRange(t *testing.T) {
	st, pl := setupPlaylist(t, 3)
	ctx := context.Background()
	for _, ord := range []int{0, -1, 4, 100} {
		if _, err := st.MovePlaylistItem(ctx, pl, ord, 1); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("ord %d: err %v, want sql.ErrNoRows", ord, err)
		}
	}
	// 未命中不应产生任何副作用
	assertOrder(t, orderOf(t, st, pl), "t1", "t2", "t3")
}
