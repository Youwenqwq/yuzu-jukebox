package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

// TestPlayerPlane covers authenticated registration, runtime state, maintenance
// commands, server-controlled Room assignment, and management authorization.
func TestPlayerPlane(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("admin", "admin123")

	for _, roomID := range []string{"pa", "pb"} {
		resp := e.post(adminToken, "/api/v1/rooms", map[string]any{"id": roomID, "name": roomID})
		resp.Body.Close()
	}
	credential, err := client.RESTCreatePlayer(
		context.Background(), e.srv.URL, adminToken, "speaker-01", "Speaker 01",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RESTBindRoomPlayer(
		context.Background(), e.srv.URL, adminToken, "pa", "speaker-01",
	); err != nil {
		t.Fatal(err)
	}

	// Player key authentication and hello; Room is resolved by the server.
	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/v1"
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	send := func(typ, ref string, data any) {
		b, _ := json.Marshal(map[string]any{"type": typ, "ref": ref, "data": data})
		if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, b, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	readRawUntil := func(typ string) map[string]any {
		for i := 0; i < 10; i++ {
			m := read()
			if m["type"] == typ {
				return m
			}
		}
		t.Fatalf("message %s not seen", typ)
		return nil
	}

	send("auth", "a1", map[string]any{"player_key": credential.Key})
	readRawUntil("auth.ok")
	send("player.hello", "a2", map[string]any{
		"device": "speaker-01", "version": "test", "caps": []string{"volume", "mute"},
	})
	hello := readRawUntil("player.hello.ok")
	playerID := hello["data"].(map[string]any)["player_id"].(string)
	if playerID != "speaker-01" {
		t.Fatalf("hello Player ID = %q", playerID)
	}
	readRawUntil("room.joined")

	// REST 列表应包含该播放端
	resp := e.get(adminToken, "/api/v1/players")
	var listed struct {
		Players []struct {
			ID     string `json:"id"`
			Device string `json:"device"`
			RoomID string `json:"room_id"`
		} `json:"players"`
	}
	decode(t, resp, &listed)
	found := false
	for _, p := range listed.Players {
		if p.ID == playerID && p.Device == "speaker-01" && p.RoomID == "pa" {
			found = true
		}
	}
	if !found {
		t.Fatalf("player not in list: %+v", listed.Players)
	}

	// REST 下发音量指令 → WS 收到 player.command
	resp = e.post(adminToken, "/api/v1/players/"+playerID+"/command",
		map[string]any{"op": "set_volume", "value": 42})
	if resp.StatusCode != 200 {
		t.Fatalf("command status %d", resp.StatusCode)
	}
	cmd := readRawUntil("player.command")
	d := cmd["data"].(map[string]any)
	if d["op"] != "set_volume" || d["value"].(float64) != 42 {
		t.Fatalf("bad command payload: %v", d)
	}

	// 上报状态 → REST 列表反映
	send("player.state", "", map[string]any{"volume": 42, "muted": false})
	time.Sleep(100 * time.Millisecond)
	resp = e.get(adminToken, "/api/v1/players")
	var listed2 struct {
		Players []struct {
			ID     string `json:"id"`
			Volume int    `json:"volume"`
		} `json:"players"`
	}
	decode(t, resp, &listed2)
	if len(listed2.Players) == 0 || listed2.Players[0].Volume != 42 {
		t.Fatalf("state not reflected: %+v", listed2.Players)
	}

	// Rebinding is the only Room migration path and persists before moving online.
	if _, err := client.RESTBindRoomPlayer(
		context.Background(), e.srv.URL, adminToken, "pb", playerID,
	); err != nil {
		t.Fatal(err)
	}
	joined := readRawUntil("room.joined")
	if joined["data"].(map[string]any)["room_id"] != "pb" {
		t.Fatalf("not migrated to pb: %v", joined)
	}
	readRawUntil("playback.changed")

	resp = e.post(adminToken, "/api/v1/players/"+playerID+"/command",
		map[string]any{"op": "join_room", "value": "pa"})
	if resp.StatusCode != 400 {
		t.Fatalf("join_room command status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// 权限：普通访客不能管理播放端
	_, guestToken := e.guestAuth("rando", "")
	resp = e.get(guestToken, "/api/v1/players")
	if resp.StatusCode != 403 {
		t.Fatalf("guest should be forbidden: %d", resp.StatusCode)
	}
}
