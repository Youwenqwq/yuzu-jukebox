package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestRoomPlayerBindingsAllowMultiplePlayersAndMoveOneAtomically(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "bindings.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, roomID := range []string{"a", "b"} {
		if err := st.CreateRoom(ctx, Room{ID: roomID, Name: roomID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.BindRoomPlayer(ctx, "a", "speaker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindRoomPlayer(ctx, "b", "speaker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindRoomPlayer(ctx, "b", "speaker-2"); err != nil {
		t.Fatal(err)
	}
	oldRoom, err := st.ListRoomPlayerBindings(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRoom) != 0 {
		t.Fatalf("old room bindings = %#v", oldRoom)
	}
	newRoom, err := st.ListRoomPlayerBindings(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(newRoom) != 2 || newRoom[0].PlayerID != "speaker-1" || newRoom[1].PlayerID != "speaker-2" {
		t.Fatalf("new room bindings = %#v", newRoom)
	}
}

func TestRoomOutputVolumeStartsUnsetAndPersists(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "output.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateRoom(ctx, Room{ID: "room", Name: "Room"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRoomOutputState(ctx, "room"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("initial output error = %v, want sql.ErrNoRows", err)
	}
	state, err := st.SetRoomOutputVolume(ctx, "room", 37)
	if err != nil {
		t.Fatal(err)
	}
	if state.RoomID != "room" || state.Volume != 37 || state.UpdatedAt == 0 {
		t.Fatalf("output state = %#v", state)
	}
}
