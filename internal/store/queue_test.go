package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestQueueRequesterNameRoundTripAndDefault(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	if err := st.CreateRoom(ctx, Room{ID: "r1", Name: "room", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	want := QueueRow{
		EntryID: "e1", TrackRef: "local:t1", Title: "Track", Artist: "Artist",
		DurationMs: 60_000, RequestedBy: "u1", RequesterName: "Alice", AddedAt: 123,
	}
	if err := st.ReplaceQueue(ctx, "r1", "", []QueueRow{want}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := st.LoadQueue(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RequesterName != want.RequesterName {
		t.Fatalf("requester_name round trip = %#v, want %q", rows, want.RequesterName)
	}

	if _, err := st.DB().ExecContext(ctx, `INSERT INTO room_queue
		(room_id, ord, entry_id, track_ref, title, artist, duration_ms, requested_by, added_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"r1", 1, "e2", "local:t2", "Legacy", "", 60_000, "u2", 456); err != nil {
		t.Fatal(err)
	}
	rows, _, err = st.LoadQueue(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].RequesterName != "" {
		t.Fatalf("legacy requester_name default = %#v, want empty string", rows)
	}
}

// 预取视界是加速层钉住集合的来源：每个房间从队列游标起前 N 条，跨房间去重，
// 越靠近正在播放的位置越靠前。
func TestRoomPrefetchHorizonTakesFirstEntriesPerRoom(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "horizon.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	for _, id := range []string{"r1", "r2"} {
		if err := st.CreateRoom(ctx, Room{ID: id, Name: id, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	row := func(entryID, ref string) QueueRow {
		return QueueRow{EntryID: entryID, TrackRef: ref, DurationMs: 1000, AddedAt: 1}
	}
	// r1 的队首是正在播放的曲目，r2 空闲、队首是下一首要放的。
	if err := st.ReplaceQueue(ctx, "r1", "e1", []QueueRow{
		row("e1", "local:playing"), row("e2", "local:next"), row("e3", "local:later"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceQueue(ctx, "r2", "", []QueueRow{
		row("e4", "local:playing"), row("e5", "local:other"),
	}); err != nil {
		t.Fatal(err)
	}

	refs, err := st.RoomPrefetchHorizon(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	// local:later 在视界外；local:playing 被两个房间共享，只算一次且最紧迫。
	want := []string{"local:playing", "local:next", "local:other"}
	if len(refs) != len(want) {
		t.Fatalf("horizon = %#v, want %#v", refs, want)
	}
	if refs[0] != "local:playing" {
		t.Fatalf("horizon = %#v, want the playing track first", refs)
	}
	for _, ref := range want {
		found := false
		for _, got := range refs {
			if got == ref {
				found = true
			}
		}
		if !found {
			t.Fatalf("horizon = %#v, missing %q", refs, ref)
		}
	}

	if refs, err = st.RoomPrefetchHorizon(ctx, 0); err != nil || len(refs) != 0 {
		t.Fatalf("horizon(0) = %#v, %v; want empty (prefetch pinning disabled)", refs, err)
	}
}
