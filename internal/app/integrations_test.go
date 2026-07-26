package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/app"
	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/config"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

const (
	testIntegrationID    = "generic-bridge"
	testIntegrationToken = "integration-secret-a"
)

type actorResolveResponse struct {
	Identity      auth.Identity `json:"identity"`
	DefaultRoomID string        `json:"default_room_id"`
	ActorToken    string        `json:"actor_token"`
	ExpiresAt     int64         `json:"expires_at"`
}

func newIntegrationEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Addr:          "127.0.0.1:0",
		DBPath:        filepath.Join(dir, "test.db"),
		MediaDir:      filepath.Join(dir, "media"),
		CacheDir:      filepath.Join(dir, "cache"),
		CacheMaxBytes: 1 << 30,
		AdminPassword: "admin123",
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, err := app.New(ctx, cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	for id, token := range map[string]string{
		testIntegrationID:  testIntegrationToken,
		"generic-bridge-b": "integration-secret-b",
	} {
		if _, err := a.Store.CreateIntegration(
			context.Background(), id, id, auth.HashIntegrationToken(token),
		); err != nil {
			t.Fatalf("create integration %s: %v", id, err)
		}
	}
	t.Cleanup(func() { _ = a.Store.Close() })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, a: a}
}

func integrationJSONRequest(t *testing.T, e *env, method, token, path string, body any) *http.Response {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func resolveIntegrationActor(t *testing.T, e *env, token string, body any) (actorResolveResponse, []byte, int) {
	t.Helper()
	resp := integrationJSONRequest(t, e, http.MethodPost, token, "/api/v1/integrations/actors/resolve", body)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out actorResolveResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("decode actor resolve: %v", err)
		}
	}
	return out, data, resp.StatusCode
}

func assertIntegrationAPIError(t *testing.T, resp *http.Response, status int, code string) {
	t.Helper()
	defer resp.Body.Close()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if resp.StatusCode != status || body.Error.Code != code {
		t.Fatalf("error response = status %d code %q, want %d %q", resp.StatusCode, body.Error.Code, status, code)
	}
}

func integrationActorBody(adapterID, scopeType, scopeID, subjectID, displayName string) map[string]any {
	return map[string]any{
		"adapter_id": adapterID,
		"scope":      map[string]any{"type": scopeType, "id": scopeID},
		"subject":    map[string]any{"id": subjectID, "display_name": displayName},
	}
}

func TestIntegrationActorGuestIsStableStandardAndShortLived(t *testing.T) {
	e := newIntegrationEnv(t)
	body := integrationActorBody("adapter", "channel", "scope-1", "subject-1", "First Name")

	_, _, status := resolveIntegrationActor(t, e, "", body)
	if status != http.StatusUnauthorized {
		t.Fatalf("missing integration token status = %d", status)
	}
	_, _, status = resolveIntegrationActor(t, e, "wrong-secret", body)
	if status != http.StatusUnauthorized {
		t.Fatalf("invalid integration token status = %d", status)
	}
	badBody := integrationActorBody("adapter", "channel", "scope-1", "subject-1", "First Name")
	badBody["unexpected"] = true
	_, _, status = resolveIntegrationActor(t, e, testIntegrationToken, badBody)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown actor field status = %d", status)
	}

	issuedAt := time.Now().UnixMilli()
	first, raw, status := resolveIntegrationActor(t, e, testIntegrationToken, body)
	if status != http.StatusOK {
		t.Fatalf("resolve guest status = %d body = %s", status, raw)
	}
	if first.Identity.ID == "" || first.Identity.Name != "First Name" || first.Identity.Kind != "guest" {
		t.Fatalf("guest identity = %#v", first.Identity)
	}
	if !testHasRole(first.Identity.Roles, auth.RoleListener) || !testHasRole(first.Identity.Roles, auth.RoleRequester) || testHasRole(first.Identity.Roles, auth.RoleRoomAdmin) {
		t.Fatalf("guest roles = %v", first.Identity.Roles)
	}
	if first.ActorToken == "" || first.ExpiresAt < issuedAt+(4*time.Minute+50*time.Second).Milliseconds() || first.ExpiresAt > time.Now().UnixMilli()+(5*time.Minute+10*time.Second).Milliseconds() {
		t.Fatalf("actor token/expiry = %q %d", first.ActorToken, first.ExpiresAt)
	}
	if first.DefaultRoomID != "" || bytes.Contains(raw, []byte(`"default_room_id"`)) {
		t.Fatalf("unbound default room was not omitted: %s", raw)
	}
	if bytes.Contains(raw, []byte(testIntegrationToken)) {
		t.Fatalf("actor response leaked integration secret: %s", raw)
	}
	var persistedExpiresAt int64
	if err := e.a.Store.DB().QueryRow(
		"SELECT expires_at FROM sessions WHERE token = ?", first.ActorToken,
	).Scan(&persistedExpiresAt); err != nil {
		t.Fatal(err)
	}
	if persistedExpiresAt != first.ExpiresAt {
		t.Fatalf("persisted expiry = %d, response expiry = %d", persistedExpiresAt, first.ExpiresAt)
	}

	resp := e.get(first.ActorToken, "/api/v1/rooms")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("standard bearer endpoint status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = e.get(testIntegrationToken, "/api/v1/rooms")
	assertIntegrationAPIError(t, resp, http.StatusUnauthorized, "unauthorized")

	renamedBody := integrationActorBody("adapter", "channel", "scope-1", "subject-1", "Renamed")
	renamed, raw, status := resolveIntegrationActor(t, e, testIntegrationToken, renamedBody)
	if status != http.StatusOK {
		t.Fatalf("resolve renamed guest status = %d body = %s", status, raw)
	}
	if renamed.Identity.ID != first.Identity.ID || renamed.Identity.Name != "Renamed" {
		t.Fatalf("renamed guest identity = %#v, first = %#v", renamed.Identity, first.Identity)
	}
	principal, err := e.a.Store.GetPrincipal(context.Background(), first.Identity.ID)
	if err != nil || principal.Name != "Renamed" {
		t.Fatalf("persisted renamed guest = %#v, %v", principal, err)
	}

	variants := []struct {
		token string
		body  map[string]any
	}{
		{token: testIntegrationToken, body: integrationActorBody("other-adapter", "channel", "scope-1", "subject-1", "Name")},
		{token: testIntegrationToken, body: integrationActorBody("adapter", "guild", "scope-1", "subject-1", "Name")},
		{token: testIntegrationToken, body: integrationActorBody("adapter", "channel", "scope-2", "subject-1", "Name")},
		{token: testIntegrationToken, body: integrationActorBody("adapter", "channel", "scope-1", "subject-2", "Name")},
		{token: "integration-secret-b", body: integrationActorBody("adapter", "channel", "scope-1", "subject-1", "Name")},
	}
	for i, variant := range variants {
		resolved, data, gotStatus := resolveIntegrationActor(t, e, variant.token, variant.body)
		if gotStatus != http.StatusOK {
			t.Fatalf("variant %d status = %d body = %s", i, gotStatus, data)
		}
		if resolved.Identity.ID == first.Identity.ID {
			t.Fatalf("variant %d did not affect deterministic guest ID", i)
		}
	}
}

func TestIntegrationManagementLinksDefaultRoomAndControllerGrant(t *testing.T) {
	e := newIntegrationEnv(t)
	_, adminToken := e.guestAuth("admin", "admin123")
	_, listenerToken := e.guestAuth("listener", "")

	resp := e.post(adminToken, "/api/v1/rooms", map[string]any{"id": "room-1", "name": "Room One"})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create room status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	scopeBody := map[string]any{
		"adapter_id": "adapter", "scope_type": "channel", "scope_id": "scope-1", "room_id": "room-1",
	}
	resp = integrationJSONRequest(t, e, http.MethodPut, testIntegrationToken, "/api/v1/integrations/"+testIntegrationID+"/scopes", scopeBody)
	assertIntegrationAPIError(t, resp, http.StatusUnauthorized, "unauthorized")
	resp = integrationJSONRequest(t, e, http.MethodPut, listenerToken, "/api/v1/integrations/"+testIntegrationID+"/scopes", scopeBody)
	assertIntegrationAPIError(t, resp, http.StatusForbidden, "forbidden")
	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken, "/api/v1/integrations/not-configured/scopes", scopeBody)
	assertIntegrationAPIError(t, resp, http.StatusNotFound, "not_found")
	missingRoomBody := map[string]any{
		"adapter_id": "adapter", "scope_type": "channel", "scope_id": "scope-1", "room_id": "missing",
	}
	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken, "/api/v1/integrations/"+testIntegrationID+"/scopes", missingRoomBody)
	assertIntegrationAPIError(t, resp, http.StatusNotFound, "not_found")

	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken, "/api/v1/integrations/"+testIntegrationID+"/scopes", scopeBody)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("scope bind status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	roomID, err := e.a.Store.ResolveExternalScopeRoom(context.Background(), testIntegrationID, "adapter", "channel", "scope-1")
	if err != nil || roomID != "room-1" {
		t.Fatalf("stored scope room = %q, %v", roomID, err)
	}

	actorBody := integrationActorBody("adapter", "channel", "scope-1", "external-user", "External User")
	guest, raw, status := resolveIntegrationActor(t, e, testIntegrationToken, actorBody)
	if status != http.StatusOK || guest.DefaultRoomID != "room-1" {
		t.Fatalf("default room resolve = %#v status %d body %s", guest, status, raw)
	}
	guestScopeBody := map[string]any{
		"adapter_id": "adapter", "scope_type": "channel", "scope_id": "scope-2", "room_id": "room-1",
	}
	resp = integrationJSONRequest(t, e, http.MethodPut, guest.ActorToken, "/api/v1/integrations/"+testIntegrationID+"/scopes", guestScopeBody)
	assertIntegrationAPIError(t, resp, http.StatusForbidden, "forbidden")

	oidcIdentity := auth.OIDCIdentity(
		auth.OIDCClaims{Sub: "oidc-subject", Username: "OIDC Before"},
		[]string{auth.RoleListener, auth.RoleRequester},
	)
	if err := e.a.Store.UpsertPrincipal(context.Background(), store.Principal{
		ID: oidcIdentity.ID, Name: oidcIdentity.Name, Kind: oidcIdentity.Kind,
		OIDCSubject: oidcIdentity.OIDCSubject, Roles: oidcIdentity.Roles, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	subjectBody := map[string]any{
		"adapter_id": "adapter", "scope_type": "channel", "scope_id": "scope-1",
		"subject_id": "external-user", "principal_id": oidcIdentity.ID,
	}
	missingPrincipalBody := map[string]any{
		"adapter_id": "adapter", "scope_type": "channel", "scope_id": "scope-1",
		"subject_id": "external-user", "principal_id": "missing",
	}
	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken, "/api/v1/integrations/"+testIntegrationID+"/subjects", missingPrincipalBody)
	assertIntegrationAPIError(t, resp, http.StatusNotFound, "not_found")
	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken, "/api/v1/integrations/"+testIntegrationID+"/subjects", subjectBody)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("subject link status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	linked, raw, status := resolveIntegrationActor(t, e, testIntegrationToken, actorBody)
	if status != http.StatusOK || linked.Identity.ID != oidcIdentity.ID || linked.Identity.Kind != "oidc" || linked.Identity.Name != "OIDC Before" {
		t.Fatalf("linked actor = %#v status %d body %s", linked, status, raw)
	}
	resp = e.get(linked.ActorToken, "/api/v1/rooms")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("linked actor standard session status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	principal, err := e.a.Store.GetPrincipal(context.Background(), oidcIdentity.ID)
	if err != nil {
		t.Fatal(err)
	}
	principal.Name = "OIDC Current"
	principal.Roles = []string{auth.RoleListener, auth.RoleRoomAdmin}
	if err := e.a.Store.UpsertPrincipal(context.Background(), principal); err != nil {
		t.Fatal(err)
	}
	current, raw, status := resolveIntegrationActor(t, e, testIntegrationToken, actorBody)
	if status != http.StatusOK || current.Identity.Name != "OIDC Current" || !testHasRole(current.Identity.Roles, auth.RoleRoomAdmin) || testHasRole(current.Identity.Roles, auth.RoleRequester) {
		t.Fatalf("current linked actor = %#v status %d body %s", current, status, raw)
	}

	principal.Active = false
	if err := e.a.Store.UpsertPrincipal(context.Background(), principal); err != nil {
		t.Fatal(err)
	}
	_, raw, status = resolveIntegrationActor(t, e, testIntegrationToken, actorBody)
	if status != http.StatusForbidden {
		t.Fatalf("disabled linked principal status = %d body = %s", status, raw)
	}
	principal.Active = true
	if err := e.a.Store.UpsertPrincipal(context.Background(), principal); err != nil {
		t.Fatal(err)
	}

	grantPath := "/api/v1/rooms/room-1/grants/" + oidcIdentity.ID
	badGrant := map[string]any{
		"room_id": "room-1", "principal_id": oidcIdentity.ID, "capability": "operator",
	}
	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken, grantPath, badGrant)
	assertIntegrationAPIError(t, resp, http.StatusBadRequest, "bad_request")
	grantBody := map[string]any{
		"room_id": "room-1", "principal_id": oidcIdentity.ID, "capability": control.CapabilityController,
	}
	resp = integrationJSONRequest(t, e, http.MethodPut, adminToken, grantPath, grantBody)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("grant status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	granted, err := e.a.Store.HasRoomGrant(context.Background(), "room-1", oidcIdentity.ID, control.CapabilityController)
	if err != nil || !granted {
		t.Fatalf("stored grant = %v, %v", granted, err)
	}
	mismatchedGrant := map[string]any{
		"room_id": "other-room", "principal_id": oidcIdentity.ID, "capability": control.CapabilityController,
	}
	resp = integrationJSONRequest(t, e, http.MethodDelete, adminToken, grantPath, mismatchedGrant)
	assertIntegrationAPIError(t, resp, http.StatusConflict, "conflict")
	resp = integrationJSONRequest(t, e, http.MethodDelete, adminToken, grantPath, grantBody)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("grant delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	granted, err = e.a.Store.HasRoomGrant(context.Background(), "room-1", oidcIdentity.ID, control.CapabilityController)
	if err != nil || granted {
		t.Fatalf("grant after delete = %v, %v", granted, err)
	}

	resp = integrationJSONRequest(t, e, http.MethodDelete, adminToken, "/api/v1/integrations/"+testIntegrationID+"/subjects", subjectBody)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("subject unlink status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	unlinked, raw, status := resolveIntegrationActor(t, e, testIntegrationToken, actorBody)
	if status != http.StatusOK || unlinked.Identity.ID == oidcIdentity.ID || unlinked.Identity.Kind != "guest" {
		t.Fatalf("unlinked actor = %#v status %d body %s", unlinked, status, raw)
	}

	resp = integrationJSONRequest(t, e, http.MethodDelete, adminToken, "/api/v1/integrations/"+testIntegrationID+"/scopes", scopeBody)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("scope unbind status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	withoutRoom, raw, status := resolveIntegrationActor(t, e, testIntegrationToken, actorBody)
	if status != http.StatusOK || withoutRoom.DefaultRoomID != "" || bytes.Contains(raw, []byte(`"default_room_id"`)) {
		t.Fatalf("actor after scope unbind = %#v status %d body %s", withoutRoom, status, raw)
	}
}

func testHasRole(roles []string, role string) bool {
	for _, candidate := range roles {
		if candidate == role {
			return true
		}
	}
	return false
}
