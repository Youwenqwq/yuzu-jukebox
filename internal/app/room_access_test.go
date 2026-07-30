package app_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/control"
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

func TestRoomAdmissionIdentityBypassesAndTrustedRoleUpdates(t *testing.T) {
	e := newEnv(t)
	_, adminToken := e.guestAuth("admission-admin", "admin123")
	controllerID, controllerToken := e.guestAuth("admission-controller", "")
	trustedID, trustedToken := e.guestAuth("admission-trusted", "")
	_, guestToken := e.guestAuth("admission-guest", "")

	for _, roomSpec := range []map[string]any{
		{"id": "admission-open", "name": "Open"},
		{
			"id":                "admission-a",
			"name":              "Protected A",
			"guest_access_mode": "static_password",
			"guest_password":    "secret-a",
		},
		{
			"id":                "admission-b",
			"name":              "Protected B",
			"guest_access_mode": "static_password",
			"guest_password":    "secret-b",
		},
	} {
		resp := e.post(adminToken, "/api/v1/rooms", roomSpec)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create room %q status = %d", roomSpec["id"], resp.StatusCode)
		}
		resp.Body.Close()
	}

	assertWSRoomAdmission(t, e, guestToken, "admission-open", "", true)
	assertWSRoomAdmission(t, e, guestToken, "admission-a", "", false)
	assertWSRoomAdmission(t, e, guestToken, "admission-a", "secret-a", true)
	assertWSRoomAdmission(t, e, adminToken, "admission-a", "", true)

	if err := e.a.Store.GrantRoomGrant(
		context.Background(), "admission-a", controllerID, control.CapabilityController,
	); err != nil {
		t.Fatal(err)
	}
	assertWSRoomAdmission(t, e, controllerToken, "admission-a", "", true)
	assertWSRoomAdmission(t, e, controllerToken, "admission-b", "", false)

	principal, err := e.a.Store.GetPrincipal(context.Background(), trustedID)
	if err != nil {
		t.Fatal(err)
	}
	principal.Roles = append(principal.Roles, "vip")
	if err := e.a.Store.UpsertPrincipal(context.Background(), principal); err != nil {
		t.Fatal(err)
	}
	assertWSRoomAdmission(t, e, trustedToken, "admission-a", "", false)
	resp := integrationJSONRequest(t, e, http.MethodPatch, adminToken, "/api/v1/rooms/admission-a", map[string]any{
		"trusted_roles": []string{"vip", "vip"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trusted_roles hot update status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	persisted, err := e.a.Store.GetRoom(context.Background(), "admission-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.TrustedRoles) != 1 || persisted.TrustedRoles[0] != "vip" {
		t.Fatalf("persisted trusted_roles = %#v, want [vip]", persisted.TrustedRoles)
	}
	assertWSRoomAdmission(t, e, trustedToken, "admission-a", "", true)

	resp = e.post(adminToken, "/api/v1/rooms", map[string]any{
		"id":                "invalid-listener-role",
		"name":              "Invalid",
		"guest_access_mode": "static_password",
		"guest_password":    "secret",
		"trusted_roles":     []string{"listener"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("listener trusted role create status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	resp = integrationJSONRequest(t, e, http.MethodPatch, adminToken, "/api/v1/rooms/admission-b", map[string]any{
		"trusted_roles": []string{"requester"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("requester trusted role update status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	persisted, err = e.a.Store.GetRoom(context.Background(), "admission-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.TrustedRoles) != 0 {
		t.Fatalf("rejected trusted_roles update persisted %#v", persisted.TrustedRoles)
	}

	resp = e.post(adminToken, "/api/v1/integrations", map[string]any{
		"id": "admission-bridge", "name": "Admission Bridge",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create admission Integration status = %d", resp.StatusCode)
	}
	var integration struct {
		Token string `json:"token"`
	}
	decode(t, resp, &integration)
	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken, "/api/v1/integrations/admission-bridge/scopes", map[string]any{
		"adapter_id": "onebot", "scope_type": "group", "scope_id": "admission-group", "room_id": "admission-a",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bind admission Integration status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	actor, raw, status := resolveIntegrationActor(t, e, integration.Token, map[string]any{
		"adapter_id": "onebot",
		"scope":      map[string]any{"type": "group", "id": "admission-group"},
		"subject":    map[string]any{"id": "member", "display_name": "Member"},
	})
	if status != http.StatusOK || actor.DefaultRoomID != "admission-a" {
		t.Fatalf("resolve admission actor = status %d actor %#v body %s", status, actor, raw)
	}
	assertWSRoomAdmission(t, e, actor.ActorToken, "admission-a", "", true)
	assertWSRoomAdmission(t, e, actor.ActorToken, "admission-b", "", false)
}

func assertWSRoomAdmission(
	t *testing.T,
	e *env,
	token, roomID, credential string,
	allowed bool,
) {
	t.Helper()
	ws := e.dialWS()
	ws.send("auth", "admission-auth", map[string]any{"session_token": token})
	ws.waitFor("auth.ok")
	ws.send("room.join", "admission-join", map[string]any{
		"room_id":  roomID,
		"password": credential,
	})
	if allowed {
		if joined := ws.waitFor("room.joined"); joined.Ref != "admission-join" {
			t.Fatalf("room %s joined ref = %q", roomID, joined.Ref)
		}
		return
	}
	assertErrCode(t, ws.waitFor("error"), "admission-join", "forbidden")
}
