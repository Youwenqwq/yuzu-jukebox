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

func openPlaylistStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPlaylistBindingRoundTripAndDetach(t *testing.T) {
	st := openPlaylistStore(t)
	ctx := context.Background()
	bound := Playlist{
		ID: "bound", Name: "远端歌单", CreatedAt: 1, UpdatedAt: 1,
		BoundProvider: "fake", BoundRemoteID: "remote-1",
		LastSyncAt: 23, LastSyncError: "old error",
	}
	if err := st.CreatePlaylist(ctx, bound); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendPlaylistItems(ctx, bound.ID, []PlaylistItem{{TrackRef: "fake:old", Title: "old"}}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := st.GetPlaylistByBinding(ctx, "fake", "remote-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("binding not found")
	}
	if got.ID != bound.ID || got.BoundProvider != "fake" || got.BoundRemoteID != "remote-1" ||
		got.LastSyncAt != 23 || got.LastSyncError != "old error" || got.TrackCount != 1 {
		t.Fatalf("binding round trip = %+v", got)
	}
	all, err := st.ListPlaylists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	boundOnly, err := st.ListBoundPlaylists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].BoundRemoteID != "remote-1" || len(boundOnly) != 1 || boundOnly[0].ID != bound.ID {
		t.Fatalf("lists: all=%+v bound=%+v", all, boundOnly)
	}

	duplicate := bound
	duplicate.ID = "duplicate"
	if err := st.CreatePlaylist(ctx, duplicate); err == nil {
		t.Fatal("duplicate provider binding unexpectedly succeeded")
	}

	if err := st.ClearPlaylistBinding(ctx, bound.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetPlaylistByBinding(ctx, "fake", "remote-1"); err != nil || ok {
		t.Fatalf("binding after detach: ok=%v err=%v", ok, err)
	}
	detached, err := st.GetPlaylist(ctx, bound.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detached.BoundProvider != "" || detached.BoundRemoteID != "" ||
		detached.LastSyncAt != 0 || detached.LastSyncError != "" {
		t.Fatalf("detached metadata = %+v", detached)
	}
	items, err := st.PlaylistItems(ctx, bound.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].TrackRef != "fake:old" {
		t.Fatalf("detach changed items: %+v", items)
	}
}

func TestReplacePlaylistItemsAtomicAndZeroBased(t *testing.T) {
	st, playlistID := setupPlaylist(t, 2)
	ctx := context.Background()
	replacement := []PlaylistItem{
		{TrackRef: "fake:new-1", Title: "new 1"},
		{TrackRef: "fake:new-2", Title: "new 2"},
		{TrackRef: "fake:new-3", Title: "new 3"},
	}
	if err := st.ReplacePlaylistItems(ctx, playlistID, replacement); err != nil {
		t.Fatal(err)
	}
	items, err := st.PlaylistItems(ctx, playlistID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("item count = %d, want 3", len(items))
	}
	for i, item := range items {
		if item.Ord != i || item.TrackRef != replacement[i].TrackRef {
			t.Fatalf("item %d = %+v, want ord=%d ref=%s", i, item, i, replacement[i].TrackRef)
		}
	}

	if _, err := st.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_bad_playlist_item
		BEFORE INSERT ON playlist_items
		WHEN NEW.track_ref = 'fake:bad'
		BEGIN
			SELECT RAISE(ABORT, 'bad item');
		END`); err != nil {
		t.Fatal(err)
	}
	err = st.ReplacePlaylistItems(ctx, playlistID, []PlaylistItem{
		{TrackRef: "fake:partial", Title: "partial"},
		{TrackRef: "fake:bad", Title: "bad"},
	})
	if err == nil {
		t.Fatal("replacement with rejected item unexpectedly succeeded")
	}
	afterFailure, err := st.PlaylistItems(ctx, playlistID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterFailure) != len(items) {
		t.Fatalf("failed replacement changed item count: %+v", afterFailure)
	}
	for i := range items {
		if afterFailure[i].Ord != items[i].Ord || afterFailure[i].TrackRef != items[i].TrackRef {
			t.Fatalf("failed replacement changed items: before=%+v after=%+v", items, afterFailure)
		}
	}
}

func TestSetPlaylistSyncResult(t *testing.T) {
	st := openPlaylistStore(t)
	ctx := context.Background()
	playlist := Playlist{
		ID: "sync-result", Name: "原名称", CreatedAt: 1, UpdatedAt: 1,
		BoundProvider: "fake", BoundRemoteID: "remote",
	}
	if err := st.CreatePlaylist(ctx, playlist); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplacePlaylistItems(ctx, playlist.ID, []PlaylistItem{{TrackRef: "fake:old", Title: "old"}}); err != nil {
		t.Fatal(err)
	}

	syncErr := errors.New("provider unavailable")
	if err := st.SetPlaylistSyncResult(ctx, playlist.ID, "不应采用", 100, syncErr); err != nil {
		t.Fatal(err)
	}
	failed, err := st.GetPlaylist(ctx, playlist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Name != "原名称" || failed.LastSyncAt != 100 || failed.LastSyncError != syncErr.Error() {
		t.Fatalf("failure result = %+v", failed)
	}
	items, err := st.PlaylistItems(ctx, playlist.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].TrackRef != "fake:old" {
		t.Fatalf("failure changed items: %+v", items)
	}

	if err := st.SetPlaylistSyncResult(ctx, playlist.ID, "远端新名称", 200, nil); err != nil {
		t.Fatal(err)
	}
	succeeded, err := st.GetPlaylist(ctx, playlist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded.Name != "远端新名称" || succeeded.LastSyncAt != 200 || succeeded.LastSyncError != "" {
		t.Fatalf("success result = %+v", succeeded)
	}
}
