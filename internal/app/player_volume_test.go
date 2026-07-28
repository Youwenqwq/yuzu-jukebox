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

func TestIntegrationActorControlsAllHeadlessPlayersInItsRoom(t *testing.T) {
	e := newIntegrationEnv(t)
	_, adminToken := e.guestAuth("admin", "admin123")
	for _, roomID := range []string{"lobby", "other"} {
		resp := e.post(adminToken, "/api/v1/rooms", map[string]any{
			"id": roomID, "name": roomID, "policy": `{}`,
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	web, err := client.Dial(ctx, e.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer web.Close()
	if _, err := web.Auth(ctx, "web-listener", ""); err != nil {
		t.Fatal(err)
	}
	if err := web.Join(ctx, "lobby", ""); err != nil {
		t.Fatal(err)
	}
	firstCredential, err := client.RESTCreatePlayer(ctx, e.srv.URL, adminToken, "speaker-1", "Speaker 1")
	if err != nil {
		t.Fatal(err)
	}
	secondCredential, err := client.RESTCreatePlayer(ctx, e.srv.URL, adminToken, "speaker-2", "Speaker 2")
	if err != nil {
		t.Fatal(err)
	}
	for _, playerID := range []string{"speaker-1", "speaker-2"} {
		if _, err := client.RESTBindRoomPlayer(ctx, e.srv.URL, adminToken, "lobby", playerID); err != nil {
			t.Fatalf("bind Player %s: %v", playerID, err)
		}
	}
	first := connectTestPlayer(t, ctx, e, firstCredential.Key)
	defer first.Close()
	second := connectTestPlayer(t, ctx, e, secondCredential.Key)
	defer func() {
		if second != nil {
			second.Close()
		}
	}()

	initialOutput, err := client.RESTRoomOutput(ctx, e.srv.URL, adminToken, "lobby")
	if err != nil {
		t.Fatal(err)
	}
	if initialOutput.Volume != nil {
		t.Fatalf("initial Room output should be unset: %#v", initialOutput)
	}

	resp := integrationJSONRequest(t, e, http.MethodPatch, adminToken,
		"/api/v1/rooms/lobby/output", map[string]any{})
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

	resp = roomOutputRequest(t, e, lobbyActor.ActorToken, "policy-disabled", 55)
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

	resp = roomOutputRequest(t, e, otherActor.ActorToken, "wrong-room", 55)
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("cross-room volume status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	_, guestToken := e.guestAuth("ordinary-listener", "")
	resp = roomOutputRequest(t, e, guestToken, "", 55)
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("ordinary listener volume status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	resp = roomOutputRequest(t, e, lobbyActor.ActorToken, "lobby-volume-1", 37)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("same-room volume status %d, want 200", resp.StatusCode)
	}
	var update client.RoomOutputUpdate
	decode(t, resp, &update)
	if update.Output.Volume == nil || *update.Output.Volume != 37 || update.Delivery.CommandsSent != 2 {
		t.Fatalf("room output update = %#v", update)
	}
	waitForPlayerVolume(t, ctx, first, 37)
	waitForPlayerVolume(t, ctx, second, 37)

	players, err := client.RESTRoomPlayers(ctx, e.srv.URL, adminToken, "lobby")
	if err != nil {
		t.Fatal(err)
	}
	if len(players) != 2 || !players[0].Bound || !players[1].Bound {
		t.Fatalf("room players = %#v", players)
	}

	second.Close()
	second = nil
	waitForPlayerOffline(t, ctx, e, adminToken, "speaker-2")
	resp = roomOutputRequest(t, e, lobbyActor.ActorToken, "lobby-volume-2", 44)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("offline convergence update status %d", resp.StatusCode)
	}
	decode(t, resp, &update)
	if update.Delivery.CommandsSent != 1 {
		t.Fatalf("offline delivery = %#v", update.Delivery)
	}
	waitForPlayerVolume(t, ctx, first, 44)

	second = connectTestPlayer(t, ctx, e, secondCredential.Key)
	waitForPlayerVolume(t, ctx, second, 44)
}

func connectTestPlayer(t *testing.T, ctx context.Context, e *env, playerKey string) *client.Client {
	t.Helper()
	player, err := client.Dial(ctx, e.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := player.AuthPlayer(ctx, playerKey); err != nil {
		player.Close()
		t.Fatal(err)
	}
	if _, err := player.PlayerHello(ctx, "test-output", "test", []string{"volume"}); err != nil {
		player.Close()
		t.Fatal(err)
	}
	return player
}

func waitForPlayerVolume(t *testing.T, ctx context.Context, player *client.Client, want int) {
	t.Helper()
	for {
		select {
		case message, ok := <-player.Events():
			if !ok {
				t.Fatal("player event stream closed")
			}
			if message.Type != "player.command" {
				continue
			}
			op, value, err := client.ParsePlayerCommand(message)
			var volume int
			if err != nil || op != "set_volume" || json.Unmarshal(value, &volume) != nil {
				continue
			}
			if volume != want {
				t.Fatalf("player volume = %d, want %d", volume, want)
			}
			return
		case <-ctx.Done():
			t.Fatalf("timed out waiting for player volume %d", want)
		}
	}
}

func waitForPlayerOffline(t *testing.T, ctx context.Context, e *env, adminToken, playerID string) {
	t.Helper()
	for {
		players, err := client.RESTPlayers(ctx, e.srv.URL, adminToken)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, player := range players {
			if player.ID == playerID {
				found = true
				if !player.Online {
					return
				}
			}
		}
		if !found {
			t.Fatalf("Player %s disappeared", playerID)
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("player %s stayed online", playerID)
		}
	}
}

func roomOutputRequest(t *testing.T, e *env, token, idempotencyKey string, volume int) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"volume": volume})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPatch, e.srv.URL+"/api/v1/rooms/lobby/output", bytes.NewReader(body))
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
