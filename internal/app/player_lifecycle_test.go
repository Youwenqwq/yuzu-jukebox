package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func TestPlayerLifecycleAndKeyAuthentication(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("admin", "admin123")

	resp := e.post(adminToken, "/api/v1/rooms", map[string]any{
		"id": "living-room", "name": "Living Room", "guest_access": map[string]any{"mode": "open"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create room status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	resp.Body.Close()

	credential, err := client.RESTCreatePlayer(
		context.Background(), e.srv.URL, adminToken, "living-speaker", "Living Speaker",
	)
	if err != nil {
		t.Fatalf("create Player: %v", err)
	}
	if credential.Player.ID != "living-speaker" || credential.Player.Name != "Living Speaker" ||
		!credential.Player.Active || credential.Player.Online || credential.Key == "" {
		t.Fatalf("created credential = %#v", credential)
	}

	bound, err := client.RESTBindRoomPlayer(
		context.Background(), e.srv.URL, adminToken, "living-room", credential.Player.ID,
	)
	if err != nil {
		t.Fatalf("bind offline Player: %v", err)
	}
	if !bound.Bound || bound.Online || bound.RoomID != "living-room" {
		t.Fatalf("offline binding = %#v", bound)
	}

	player := e.dialWS()
	player.send("auth", "auth-1", map[string]any{"player_key": credential.Key})
	authOK := player.waitFor("auth.ok")
	var authData struct {
		Identity struct {
			Kind  string   `json:"kind"`
			Roles []string `json:"roles"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(authOK.Data, &authData); err != nil {
		t.Fatal(err)
	}
	if authData.Identity.Kind != "player" || len(authData.Identity.Roles) != 0 {
		t.Fatalf("Player identity = %#v", authData.Identity)
	}

	player.send("player.hello", "hello-1", map[string]any{
		"device": "test-output", "version": "test", "caps": []string{"volume", "mute"},
	})
	helloOK := player.waitFor("player.hello.ok")
	var helloData struct {
		PlayerID string `json:"player_id"`
	}
	if err := json.Unmarshal(helloOK.Data, &helloData); err != nil {
		t.Fatal(err)
	}
	if helloData.PlayerID != "living-speaker" {
		t.Fatalf("registered Player = %q, want living-speaker", helloData.PlayerID)
	}
	joined := player.waitFor("room.joined")
	var joinedData struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(joined.Data, &joinedData); err != nil {
		t.Fatal(err)
	}
	if joinedData.RoomID != "living-room" {
		t.Fatalf("joined Room = %q, want living-room", joinedData.RoomID)
	}

	player.send("room.leave", "leave-1", map[string]any{})
	assertWSErrorCode(t, player.waitFor("error"), "forbidden")

	online := waitForPlayerOnline(t, e.srv.URL, adminToken, "living-speaker", true)
	if online.RoomID != "living-room" || online.Device != "test-output" {
		t.Fatalf("online Player = %#v", online)
	}

	rotated, err := client.RESTRotatePlayerKey(context.Background(), e.srv.URL, adminToken, "living-speaker")
	if err != nil {
		t.Fatalf("rotate Player key: %v", err)
	}
	if rotated.Key == "" || rotated.Key == credential.Key {
		t.Fatalf("rotated key was not replaced")
	}
	waitForPlayerOnline(t, e.srv.URL, adminToken, "living-speaker", false)
	assertPlayerKeyRejected(t, e, credential.Key)

	player = e.dialWS()
	player.send("auth", "auth-2", map[string]any{"player_key": rotated.Key})
	player.waitFor("auth.ok")
	player.send("player.hello", "hello-2", map[string]any{"device": "test-output", "version": "test"})
	player.waitFor("player.hello.ok")
	player.waitFor("room.joined")

	active := false
	if _, err := client.RESTUpdatePlayer(context.Background(), e.srv.URL, adminToken, "living-speaker", client.PlayerUpdate{Active: &active}); err != nil {
		t.Fatalf("disable Player: %v", err)
	}
	waitForPlayerOnline(t, e.srv.URL, adminToken, "living-speaker", false)
	assertPlayerKeyRejected(t, e, rotated.Key)

	active = true
	if _, err := client.RESTUpdatePlayer(context.Background(), e.srv.URL, adminToken, "living-speaker", client.PlayerUpdate{Active: &active}); err != nil {
		t.Fatalf("enable Player: %v", err)
	}
	player = e.dialWS()
	player.send("auth", "auth-3", map[string]any{"player_key": rotated.Key})
	player.waitFor("auth.ok")
	player.send("player.hello", "hello-3", map[string]any{"device": "test-output", "version": "test"})
	player.waitFor("player.hello.ok")
	player.waitFor("room.joined")

	if err := client.RESTDeletePlayer(context.Background(), e.srv.URL, adminToken, "living-speaker"); err != nil {
		t.Fatalf("delete Player: %v", err)
	}
	assertPlayerKeyRejected(t, e, rotated.Key)
	roomPlayers, err := client.RESTRoomPlayers(context.Background(), e.srv.URL, adminToken, "living-room")
	if err != nil {
		t.Fatalf("list Room Players: %v", err)
	}
	if len(roomPlayers) != 0 {
		t.Fatalf("Room bindings survived Player deletion: %#v", roomPlayers)
	}
}

func waitForPlayerOnline(
	t *testing.T,
	server, token, playerID string,
	want bool,
) client.PlayerInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		player, err := client.RESTGetPlayer(context.Background(), server, token, playerID)
		if err == nil && player.Online == want {
			return player
		}
		time.Sleep(10 * time.Millisecond)
	}
	player, err := client.RESTGetPlayer(context.Background(), server, token, playerID)
	if err != nil {
		t.Fatalf("get Player while waiting for online=%v: %v", want, err)
	}
	t.Fatalf("Player online = %v, want %v", player.Online, want)
	return client.PlayerInfo{}
}

func assertPlayerKeyRejected(t *testing.T, e *env, key string) {
	t.Helper()
	candidate := e.dialWS()
	candidate.send("auth", "auth-rejected", map[string]any{"player_key": key})
	assertWSErrorCode(t, candidate.waitFor("error"), "unauthorized")
	candidate.conn.CloseNow()
}

func assertWSErrorCode(t *testing.T, message wsMsg, want string) {
	t.Helper()
	var data struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Code != want {
		t.Fatalf("WS error code = %q, want %q", data.Code, want)
	}
}
