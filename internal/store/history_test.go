package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPlayHistoryByRequester(t *testing.T) {
	st := openHistoryStore(t)
	ctx := context.Background()
	seedPlayHistory(t, st)

	rows, err := st.PlayHistoryByRequester(ctx, "alice", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	wantRefs := []string{"ncm:a", "ncm:c", "ncm:b", "ncm:a"}
	if len(rows) != len(wantRefs) {
		t.Fatalf("history length = %d, want %d: %#v", len(rows), len(wantRefs), rows)
	}
	for i, want := range wantRefs {
		if rows[i].TrackRef != want {
			t.Fatalf("history[%d].track_ref = %q, want %q", i, rows[i].TrackRef, want)
		}
		if rows[i].RequestedBy != "alice" {
			t.Fatalf("history leaked requester %q at row %d", rows[i].RequestedBy, i)
		}
	}
	if rows[0].RoomID != "room-2" || rows[1].RoomID != "room-1" {
		t.Fatalf("history room ids = %q, %q; want room-2, room-1", rows[0].RoomID, rows[1].RoomID)
	}
	if rows[0].Artist != "artist-1" || rows[2].Artist != "artist-2" {
		t.Fatalf("history artists = %q, %q; want artist-1, artist-2", rows[0].Artist, rows[2].Artist)
	}

	page, err := st.PlayHistoryByRequester(ctx, "alice", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].TrackRef != "ncm:c" || page[1].TrackRef != "ncm:b" {
		t.Fatalf("history page = %#v, want ncm:c then ncm:b", page)
	}
}

func TestHotTracks(t *testing.T) {
	st := openHistoryStore(t)
	ctx := context.Background()
	seedPlayHistory(t, st)

	all, err := st.HotTracks(ctx, 0, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all-time hot tracks = %#v, want 3 entries", all)
	}
	if all[0].TrackRef != "ncm:a" || all[0].PlayCount != 3 || all[0].Title != "A new" || all[0].LastPlayedAt != 500 {
		t.Fatalf("all-time first track = %#v, want aggregated ncm:a with latest title", all[0])
	}
	if all[0].Artist != "artist-1" || all[1].Artist != "artist-2" {
		t.Fatalf("hot track artists = %q, %q; want artist-1, artist-2", all[0].Artist, all[1].Artist)
	}
	if all[1].TrackRef != "ncm:b" || all[1].PlayCount != 2 || all[1].LastPlayedAt != 600 {
		t.Fatalf("all-time second track = %#v, want ncm:b count 2", all[1])
	}

	page, err := st.HotTracks(ctx, 0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].TrackRef != "ncm:b" {
		t.Fatalf("hot tracks page = %#v, want second-ranked ncm:b", page)
	}

	recent, err := st.HotTracks(ctx, 250, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 3 {
		t.Fatalf("recent hot tracks = %#v, want 3 entries", recent)
	}
	if recent[0].TrackRef != "ncm:b" || recent[0].PlayCount != 2 || recent[0].LastPlayedAt != 600 {
		t.Fatalf("recent first track = %#v, want ncm:b count 2", recent[0])
	}
	if recent[1].TrackRef != "ncm:a" || recent[1].PlayCount != 1 || recent[1].Title != "A new" {
		t.Fatalf("recent second track = %#v, want ncm:a count 1", recent[1])
	}
}


func openHistoryStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "history.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedPlayHistory(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	rows := []struct {
		roomID, ref, title, artist, requester, reason string
		startedAt                                     int64
	}{
		{"room-1", "ncm:a", "A old", "artist-1", "alice", "finished", 100},
		{"room-2", "ncm:a", "A middle", "artist-1", "bob", "finished", 200},
		{"room-2", "ncm:b", "B old", "artist-2", "alice", "finished", 300},
		{"room-1", "ncm:c", "C", "artist-1", "alice", "skipped", 350},
		{"room-2", "ncm:a", "A new", "artist-1", "alice", "finished", 500},
		{"room-1", "ncm:b", "B new", "artist-2", "bob", "error", 600},
	}
	for _, row := range rows {
		if err := st.AddPlayHistory(ctx, row.roomID, row.ref, row.title, row.artist, row.requester, row.startedAt, row.startedAt+1, row.reason); err != nil {
			t.Fatal(err)
		}
	}
}
