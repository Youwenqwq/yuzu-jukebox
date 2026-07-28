package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestRoomPlayerBindingMovesPlayerAtomically(t *testing.T) {
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
	if _, err := st.GetRoomPlayerBinding(ctx, "a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old room binding error = %v, want sql.ErrNoRows", err)
	}
	binding, err := st.GetRoomPlayerBinding(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if binding.PlayerID != "speaker-1" {
		t.Fatalf("new binding = %#v", binding)
	}
}
