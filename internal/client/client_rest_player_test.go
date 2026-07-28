package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRESTRoomOutputAndPlayerOperations(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "Bearer actor-token" {
			t.Errorf("Authorization = %q", got)
		}
		switch calls {
		case 1:
			if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/v1/rooms/room%2Fone/output" {
				t.Errorf("output request = %s %s", r.Method, r.URL.EscapedPath())
			}
			json.NewEncoder(w).Encode(map[string]any{"output": map[string]any{"volume": nil}})
		case 2:
			if r.Method != http.MethodPut || r.URL.EscapedPath() != "/api/v1/rooms/room%2Fone/players/speaker-1" {
				t.Errorf("bind request = %s %s", r.Method, r.URL.EscapedPath())
			}
			json.NewEncoder(w).Encode(map[string]any{
				"player": map[string]any{"id": "speaker-1", "bound": true, "online": true, "room_id": "room/one"},
			})
		case 3:
			if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/v1/rooms/room%2Fone/players" {
				t.Errorf("players request = %s %s", r.Method, r.URL.EscapedPath())
			}
			json.NewEncoder(w).Encode(map[string]any{
				"players": []map[string]any{{"id": "speaker-1", "bound": true, "online": true}},
			})
		case 4:
			if r.Method != http.MethodPatch || r.URL.EscapedPath() != "/api/v1/rooms/room%2Fone/output" {
				t.Errorf("volume request = %s %s", r.Method, r.URL.EscapedPath())
			}
			if got := r.Header.Get("Idempotency-Key"); got != "event-42" {
				t.Errorf("Idempotency-Key = %q", got)
			}
			var body struct {
				Volume int `json:"volume"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Volume != 37 {
				t.Errorf("volume body = %#v, %v", body, err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"output":   map[string]any{"volume": 37, "updated_at": 123},
				"delivery": map[string]any{"commands_sent": 2},
			})
		case 5:
			if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/api/v1/rooms/room%2Fone/players/speaker-1" {
				t.Errorf("unbind request = %s %s", r.Method, r.URL.EscapedPath())
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			t.Errorf("unexpected request %d", calls)
		}
	}))
	defer server.Close()

	output, err := RESTRoomOutput(context.Background(), server.URL, "actor-token", "room/one")
	if err != nil {
		t.Fatal(err)
	}
	if output.Volume != nil {
		t.Fatalf("initial output = %#v", output)
	}
	player, err := RESTBindRoomPlayer(context.Background(), server.URL, "actor-token", "room/one", "speaker-1")
	if err != nil {
		t.Fatal(err)
	}
	if player.ID != "speaker-1" || !player.Bound || !player.Online || player.RoomID != "room/one" {
		t.Fatalf("bound player = %#v", player)
	}
	players, err := RESTRoomPlayers(context.Background(), server.URL, "actor-token", "room/one")
	if err != nil || len(players) != 1 || players[0].ID != "speaker-1" {
		t.Fatalf("room players = %#v, %v", players, err)
	}
	ctx := WithIdempotencyKey(context.Background(), "event-42")
	update, err := RESTRoomOutputSetVolume(ctx, server.URL, "actor-token", "room/one", 37)
	if err != nil {
		t.Fatal(err)
	}
	if update.Output.Volume == nil || *update.Output.Volume != 37 || update.Delivery.CommandsSent != 2 {
		t.Fatalf("output update = %#v", update)
	}
	if err := RESTUnbindRoomPlayer(context.Background(), server.URL, "actor-token", "room/one", "speaker-1"); err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatalf("calls = %d", calls)
	}
}
