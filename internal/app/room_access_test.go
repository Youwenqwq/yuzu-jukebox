package app_test

import (
	"net/http"
	"testing"
)

type roomAccessCodeResponse struct {
	RoomID     string `json:"room_id"`
	AccessCode struct {
		Code          string `json:"code"`
		PeriodSeconds int64  `json:"period_seconds"`
		ValidFrom     int64  `json:"valid_from"`
		ExpiresAt     int64  `json:"expires_at"`
	} `json:"access_code"`
}

func TestRotatingRoomAccessAndIntegrationDelivery(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("admin", "admin123")
	_, guestToken := e.guestAuth("guest", "")

	resp := e.post(adminToken, "/api/v1/rooms", map[string]any{
		"id": "daily", "name": "Daily",
		"guest_access_mode": "rotating_code", "guest_code_period_seconds": 86400,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create rotating Room status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.get(adminToken, "/api/v1/rooms/daily/access-code")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("admin code response = %d cache-control=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var current roomAccessCodeResponse
	decode(t, resp, &current)
	if len(current.AccessCode.Code) != 14 || current.AccessCode.PeriodSeconds != 86400 ||
		current.AccessCode.ExpiresAt <= current.AccessCode.ValidFrom {
		t.Fatalf("access code = %#v", current.AccessCode)
	}

	resp = e.get(guestToken, "/api/v1/rooms/daily/access-code")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ordinary guest code response = %d", resp.StatusCode)
	}
	resp.Body.Close()

	ws := e.dialWS()
	ws.send("auth", "auth", map[string]any{"session_token": guestToken})
	ws.waitFor("auth.ok")
	ws.send("room.join", "wrong", map[string]any{"room_id": "daily", "password": "0000-0000-0000"})
	if msg := ws.waitFor("error"); msg.Ref != "wrong" {
		t.Fatalf("wrong-code response ref = %q", msg.Ref)
	}
	ws.send("room.join", "current", map[string]any{"room_id": "daily", "password": current.AccessCode.Code})
	ws.waitFor("room.joined")

	resp = integrationJSONRequest(t, e, http.MethodPatch, adminToken, "/api/v1/rooms/daily", map[string]any{
		"guest_access_mode": "open",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open hot update status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	openWS := e.dialWS()
	openWS.send("auth", "auth", map[string]any{"session_token": guestToken})
	openWS.waitFor("auth.ok")
	openWS.send("room.join", "open", map[string]any{"room_id": "daily"})
	openWS.waitFor("room.joined")

	resp = integrationJSONRequest(t, e, http.MethodPatch, adminToken, "/api/v1/rooms/daily", map[string]any{
		"guest_access_mode": "rotating_code", "guest_code_period_seconds": 86400,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotating hot update status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.post(adminToken, "/api/v1/integrations", map[string]any{
		"id": "room-code-bridge", "name": "Room Code Bridge",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create Integration status = %d", resp.StatusCode)
	}
	var created struct {
		Token string `json:"token"`
	}
	decode(t, resp, &created)
	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken, "/api/v1/integrations/room-code-bridge/scopes", map[string]any{
		"adapter_id": "onebot", "scope_type": "group", "scope_id": "group-1", "room_id": "daily",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bind Integration scope status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	actor, raw, status := resolveIntegrationActor(t, e, created.Token, map[string]any{
		"adapter_id": "onebot",
		"scope":      map[string]any{"type": "group", "id": "group-1"},
		"subject":    map[string]any{"id": "member-1", "display_name": "Member One"},
	})
	if status != http.StatusOK || actor.DefaultRoomID != "daily" {
		t.Fatalf("resolve actor = %d %#v %s", status, actor, raw)
	}
	resp = e.get(actor.ActorToken, "/api/v1/rooms/daily/access-code")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Integration actor code response = %d", resp.StatusCode)
	}
	var delivered roomAccessCodeResponse
	decode(t, resp, &delivered)
	if delivered.AccessCode.Code != current.AccessCode.Code {
		t.Fatalf("delivered code = %q, admin code = %q", delivered.AccessCode.Code, current.AccessCode.Code)
	}
}
