package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRESTRoomPlayerOperations(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "Bearer actor-token" {
			t.Errorf("Authorization = %q", got)
		}
		switch calls {
		case 1:
			if r.Method != http.MethodPut || r.URL.EscapedPath() != "/api/v1/rooms/room%2Fone/player" {
				t.Errorf("bind request = %s %s", r.Method, r.URL.EscapedPath())
			}
			var body struct {
				PlayerID string `json:"player_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PlayerID != "speaker-1" {
				t.Errorf("bind body = %#v, %v", body, err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"player": map[string]any{"id": "speaker-1", "online": true, "room_id": "room/one"},
			})
		case 2:
			if r.Method != http.MethodPost || r.URL.EscapedPath() != "/api/v1/rooms/room%2Fone/player/volume" {
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
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case 3:
			if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/api/v1/rooms/room%2Fone/player" {
				t.Errorf("unbind request = %s %s", r.Method, r.URL.EscapedPath())
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			t.Errorf("unexpected request %d", calls)
		}
	}))
	defer server.Close()

	player, err := RESTBindRoomPlayer(context.Background(), server.URL, "actor-token", "room/one", "speaker-1")
	if err != nil {
		t.Fatal(err)
	}
	if player.ID != "speaker-1" || !player.Online || player.RoomID != "room/one" {
		t.Fatalf("bound player = %#v", player)
	}
	ctx := WithIdempotencyKey(context.Background(), "event-42")
	if err := RESTRoomPlayerSetVolume(ctx, server.URL, "actor-token", "room/one", 37); err != nil {
		t.Fatal(err)
	}
	if err := RESTUnbindRoomPlayer(context.Background(), server.URL, "actor-token", "room/one"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
}
