package app_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func bearerJSONRequest(
	t *testing.T,
	e *env,
	method, path, token string,
	body any,
	idempotencyKey string,
) (*http.Response, []byte) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, responseBody
}

func TestIntegrationPendingEntryStreamTicket(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("ticket-admin", "admin123")
	createControlRoom(t, e, adminToken, "ticket-room")

	resp := e.post(adminToken, "/api/v1/integrations", map[string]any{
		"id": "ticket-bridge", "name": "Ticket Bridge",
	})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create integration status = %d, body = %s", resp.StatusCode, body)
	}
	var created struct {
		Token string `json:"token"`
	}
	decode(t, resp, &created)
	if created.Token == "" {
		t.Fatal("create integration returned an empty token")
	}

	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken,
		"/api/v1/integrations/ticket-bridge/scopes", map[string]any{
			"adapter_id": "ticket-adapter", "scope_type": "group",
			"scope_id": "ticket-group", "room_id": "ticket-room",
		})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("bind scope status = %d, body = %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	actor, raw, status := resolveIntegrationActor(t, e, created.Token,
		integrationActorBody("ticket-adapter", "group", "ticket-group", "member", "Member"))
	if status != http.StatusOK || actor.DefaultRoomID != "ticket-room" {
		t.Fatalf("resolve actor = status %d body %s", status, raw)
	}

	firstRef := uploadTrack(t, e, adminToken, "ticket-first")
	secondRef := uploadTrack(t, e, adminToken, "ticket-second")

	resp, body := bearerJSONRequest(t, e, http.MethodPost, "/api/v1/rooms/ticket-room/queue",
		actor.ActorToken, map[string]any{"track_refs": []string{firstRef, secondRef}},
		"astrbot-v1:ticket-queue")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queue status = %d body = %s", resp.StatusCode, body)
	}
	var queued struct {
		EntryIDs []string `json:"entry_ids"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || len(queued.EntryIDs) != 2 {
		t.Fatalf("queue response = %s, err = %v", body, err)
	}

	resp, body = bearerJSONRequest(t, e, http.MethodPost,
		"/api/v1/rooms/ticket-room/stream-tickets", actor.ActorToken,
		map[string]any{"entry_id": queued.EntryIDs[1]}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ticket status = %d body = %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var ticket struct {
		EntryID    string `json:"entry_id"`
		TrackRef   string `json:"track_ref"`
		DurationMs int64  `json:"duration_ms"`
		StreamURL  string `json:"stream_url"`
		ExpiresAt  int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.EntryID != queued.EntryIDs[1] || ticket.TrackRef != secondRef {
		t.Fatalf("ticket = %+v, want entry %s ref %s", ticket, queued.EntryIDs[1], secondRef)
	}
	if ticket.DurationMs <= 0 || ticket.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("ticket duration/expiry = %+v", ticket)
	}
	if !strings.HasPrefix(ticket.StreamURL, "/stream/v1/"+secondRef+"?ticket=") {
		t.Fatalf("ticket stream_url = %q", ticket.StreamURL)
	}

	streamResp, err := http.Get(e.srv.URL + ticket.StreamURL)
	if err != nil {
		t.Fatal(err)
	}
	streamBody, _ := io.ReadAll(streamResp.Body)
	streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK || len(streamBody) == 0 {
		t.Fatalf("stream = status %d bytes %d", streamResp.StatusCode, len(streamBody))
	}

	_, guestToken := e.guestAuth("ticket-guest", "")
	resp, body = bearerJSONRequest(t, e, http.MethodPost,
		"/api/v1/rooms/ticket-room/stream-tickets", guestToken,
		map[string]any{"entry_id": queued.EntryIDs[0]}, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ordinary guest ticket status = %d body = %s", resp.StatusCode, body)
	}

	other, raw, status := resolveIntegrationActor(t, e, created.Token,
		integrationActorBody("ticket-adapter", "group", "ticket-group", "other", "Other"))
	if status != http.StatusOK {
		t.Fatalf("resolve other actor = status %d body %s", status, raw)
	}
	resp, body = bearerJSONRequest(t, e, http.MethodPost,
		"/api/v1/rooms/ticket-room/stream-tickets", other.ActorToken,
		map[string]any{"entry_id": queued.EntryIDs[0]}, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign entry ticket status = %d body = %s", resp.StatusCode, body)
	}

	resp, body = bearerJSONRequest(t, e, http.MethodPost,
		"/api/v1/rooms/ticket-room/stream-tickets", actor.ActorToken,
		map[string]any{"entry_id": "missing"}, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing entry ticket status = %d body = %s", resp.StatusCode, body)
	}
	resp, body = bearerJSONRequest(t, e, http.MethodPost,
		"/api/v1/rooms/ticket-room/stream-tickets", actor.ActorToken,
		map[string]any{}, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing entry_id status = %d body = %s", resp.StatusCode, body)
	}
}
