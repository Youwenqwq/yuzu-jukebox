package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func TestIntegrationActorControlsOnlyItsRoomPlayerVolume(t *testing.T) {
	e := newIntegrationEnv(t)
	_, adminToken := e.guestAuth("admin", "admin123")
	for _, roomID := range []string{"lobby", "other"} {
		resp := e.post(adminToken, "/api/v1/rooms", map[string]any{
			"id": roomID, "name": roomID,
			"policy": `{}`,
		})
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("create room %s status %d", roomID, resp.StatusCode)
		}
		resp.Body.Close()
		resp = integrationJSONRequest(t, e, http.MethodPut, adminToken,
			"/api/v1/integrations/"+testIntegrationID+"/scopes", map[string]any{
				"adapter_id": "astrbot", "scope_type": "group",
				"scope_id": roomID + "-scope", "room_id": roomID,
			})
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("bind scope %s status %d", roomID, resp.StatusCode)
		}
		resp.Body.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	player, err := client.Dial(ctx, e.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer player.Close()
	if _, err := player.Auth(ctx, "lobby-speaker", ""); err != nil {
		t.Fatal(err)
	}
	if err := player.Join(ctx, "lobby", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := player.PlayerHello(ctx, "speaker-lobby", "Lobby speaker", "test", []string{"volume"}); err != nil {
		t.Fatal(err)
	}
	resp := integrationJSONRequest(t, e, http.MethodPut, adminToken,
		"/api/v1/rooms/lobby/player", map[string]any{"player_id": "speaker-lobby"})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("bind player status %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.post(adminToken, "/api/v1/rooms/lobby/player/volume", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("missing volume status %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	lobbyActor, _, status := resolveIntegrationActor(t, e, testIntegrationToken,
		integrationActorBody("astrbot", "group", "lobby-scope", "member-1", "Member"))
	if status != http.StatusOK {
		t.Fatalf("resolve lobby actor status %d", status)
	}
	otherActor, _, status := resolveIntegrationActor(t, e, testIntegrationToken,
		integrationActorBody("astrbot", "group", "other-scope", "member-1", "Member"))
	if status != http.StatusOK {
		t.Fatalf("resolve other actor status %d", status)
	}

	resp = playerVolumeRequest(t, e, lobbyActor.ActorToken, "policy-disabled", 55)
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("disabled policy volume status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	resp = integrationJSONRequest(t, e, http.MethodPatch, adminToken, "/api/v1/rooms/lobby",
		map[string]any{"policy": `{"member_player_volume":true}`})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("enable member volume status %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = playerVolumeRequest(t, e, otherActor.ActorToken, "wrong-room", 55)
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("cross-room volume status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	_, guestToken := e.guestAuth("ordinary-listener", "")
	resp = playerVolumeRequest(t, e, guestToken, "", 55)
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("ordinary listener volume status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	resp = playerVolumeRequest(t, e, lobbyActor.ActorToken, "lobby-volume-1", 37)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("same-room volume status %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	for {
		select {
		case message := <-player.Events():
			if message.Type != "player.command" {
				continue
			}
			op, value, err := client.ParsePlayerCommand(message)
			if err != nil || op != "set_volume" || string(value) != "37" {
				t.Fatalf("player command = %q %s, %v", op, value, err)
			}
			return
		case <-ctx.Done():
			t.Fatal("timed out waiting for player.command")
		}
	}
}

func playerVolumeRequest(t *testing.T, e *env, token, idempotencyKey string, volume int) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"volume": volume})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/rooms/lobby/player/volume", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
