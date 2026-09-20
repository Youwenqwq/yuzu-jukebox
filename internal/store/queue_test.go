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
