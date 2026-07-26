package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
)

const (
	managementRoomID             = "main"
	managementIntegrationID      = "bridge-z"
	managementIntegrationSecret  = "integration-secret-z"
	managementIntegrationSecretA = "integration-secret-a"
	managementOIDCSubject        = "private-oidc-subject"
)

type managementQueryFixture struct {
	handler    http.Handler
	adminToken string
	reader     auth.Identity
	readerTok  string
	otherTok   string
}

func setupManagementQueries(t *testing.T) managementQueryFixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = st.Close()
	})

	authm := auth.NewManager("", st)
	integrations, err := auth.NewIntegrationRegistry([]auth.IntegrationCredential{
		{ID: managementIntegrationID, Token: managementIntegrationSecret},
		{ID: "bridge-a", Token: managementIntegrationSecretA},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry()
	roomCache := cache.New(filepath.Join(root, "cache"), 1<<20, st, reg)
	rooms := room.NewManager(ctx, st, authm, roomCache, reg)
	roomRow := store.Room{ID: managementRoomID, Name: "Main", CreatedAt: 1}
	if err := st.CreateRoom(ctx, roomRow); err != nil {
		t.Fatal(err)
	}
	rooms.Spawn(roomRow)

	adminToken := authm.IssueSession(auth.Identity{
		ID: "principal-admin", Name: "Admin", Kind: "password",
		Roles: []string{auth.RoleListener, auth.RoleRoomAdmin},
	})
	reader := auth.Identity{
		ID: "principal-reader", Name: "Reader", Kind: "guest",
		Roles: []string{auth.RoleListener},
	}
	readerTok := authm.IssueSession(reader)
	otherTok := authm.IssueSession(auth.Identity{
		ID: "principal-other", Name: "Other", Kind: "guest",
		Roles: []string{auth.RoleListener},
	})
	alice := store.Principal{
		ID: "principal-alice", Name: "Alice", Kind: "oidc",
		OIDCSubject: managementOIDCSubject, Roles: []string{auth.RoleListener}, Active: true,
	}
	if err := st.UpsertPrincipal(ctx, alice); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []store.ExternalScopeRoom{
		{IntegrationID: managementIntegrationID, AdapterID: "zeta", ScopeType: "group", ScopeID: "2", RoomID: managementRoomID},
		{IntegrationID: managementIntegrationID, AdapterID: "alpha", ScopeType: "group", ScopeID: "1", RoomID: managementRoomID},
	} {
		if err := st.BindExternalScopeRoom(ctx, binding.IntegrationID, binding.AdapterID, binding.ScopeType, binding.ScopeID, binding.RoomID); err != nil {
			t.Fatal(err)
		}
	}
	for _, link := range []store.ExternalIdentityLink{
		{IntegrationID: managementIntegrationID, AdapterID: "zeta", ScopeType: "group", ScopeID: "2", SubjectID: "user-z", PrincipalID: alice.ID},
		{IntegrationID: managementIntegrationID, AdapterID: "alpha", ScopeType: "group", ScopeID: "1", SubjectID: "user-a", PrincipalID: reader.ID},
	} {
		if err := st.UpsertExternalIdentityLink(ctx, link.IntegrationID, link.AdapterID, link.ScopeType, link.ScopeID, link.SubjectID, link.PrincipalID); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.GrantRoomGrant(ctx, managementRoomID, reader.ID, control.CapabilityController); err != nil {
		t.Fatal(err)
	}

	controls := control.NewService(rooms, reg, control.NewAuthorizer(st))
	s := &Server{
		st: st, authm: authm, integrations: integrations, rooms: rooms,
		reg: reg, cache: roomCache, controls: controls, ws: wsapi.NewServer(authm, controls),
	}
	return managementQueryFixture{
		handler: s.Handler(), adminToken: adminToken,
		reader: reader, readerTok: readerTok, otherTok: otherTok,
	}
}

func managementQueryGET(t *testing.T, fixture managementQueryFixture, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	fixture.handler.ServeHTTP(rec, req)
	return rec
}

func TestRoomCapabilitiesUseStandardSessionAndAuthorizer(t *testing.T) {
	fixture := setupManagementQueries(t)

	for _, test := range []struct {
		name       string
		token      string
		controller bool
	}{
		{name: "granted ordinary user", token: fixture.readerTok, controller: true},
		{name: "ordinary user without grant", token: fixture.otherTok, controller: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := managementQueryGET(t, fixture, "/api/v1/rooms/main/capabilities", test.token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Capabilities control.RoomCapabilities `json:"capabilities"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Capabilities.Controller != test.controller {
				t.Fatalf("capabilities = %#v", body.Capabilities)
			}
		})
	}

	rec := managementQueryGET(t, fixture, "/api/v1/rooms/missing/capabilities", fixture.readerTok)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing room status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestManagementQueriesRequireRoomAdminAndDoNotLeakSecrets(t *testing.T) {
	fixture := setupManagementQueries(t)
	adminPaths := []string{
		"/api/v1/integrations",
		"/api/v1/integrations/bridge-z/scopes",
		"/api/v1/integrations/bridge-z/subjects",
		"/api/v1/rooms/main/grants",
		"/api/v1/principals",
	}
	for _, path := range adminPaths {
		rec := managementQueryGET(t, fixture, path, fixture.readerTok)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s as reader status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
	}

	rec := managementQueryGET(t, fixture, "/api/v1/integrations", fixture.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("integrations status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), managementIntegrationSecret) || strings.Contains(rec.Body.String(), managementIntegrationSecretA) {
		t.Fatalf("integration response leaked token: %s", rec.Body.String())
	}
	var integrations struct {
		Integrations []integrationInfoResponse `json:"integrations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &integrations); err != nil {
		t.Fatal(err)
	}
	if len(integrations.Integrations) != 2 || integrations.Integrations[0].ID != "bridge-a" || integrations.Integrations[1].ID != managementIntegrationID {
		t.Fatalf("integrations = %#v", integrations.Integrations)
	}

	rec = managementQueryGET(t, fixture, "/api/v1/integrations/bridge-z/scopes", fixture.adminToken)
	var scopes struct {
		Scopes []integrationScopeResponse `json:"scopes"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &scopes) != nil {
		t.Fatalf("scopes status/body = %d %s", rec.Code, rec.Body.String())
	}
	if len(scopes.Scopes) != 2 || scopes.Scopes[0].AdapterID != "alpha" || scopes.Scopes[1].AdapterID != "zeta" {
		t.Fatalf("scopes = %#v", scopes.Scopes)
	}

	rec = managementQueryGET(t, fixture, "/api/v1/integrations/bridge-z/subjects", fixture.adminToken)
	var subjects struct {
		Subjects []integrationSubjectResponse `json:"subjects"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &subjects) != nil {
		t.Fatalf("subjects status/body = %d %s", rec.Code, rec.Body.String())
	}
	if len(subjects.Subjects) != 2 || subjects.Subjects[0].AdapterID != "alpha" || subjects.Subjects[1].AdapterID != "zeta" {
		t.Fatalf("subjects = %#v", subjects.Subjects)
	}

	rec = managementQueryGET(t, fixture, "/api/v1/rooms/main/grants", fixture.adminToken)
	var grants struct {
		Grants []roomGrantRequest `json:"grants"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &grants) != nil {
		t.Fatalf("grants status/body = %d %s", rec.Code, rec.Body.String())
	}
	if len(grants.Grants) != 1 || grants.Grants[0].PrincipalID != fixture.reader.ID || grants.Grants[0].Capability != control.CapabilityController {
		t.Fatalf("grants = %#v", grants.Grants)
	}
	if strings.Contains(rec.Body.String(), "granted_at") {
		t.Fatalf("grant response leaked storage metadata: %s", rec.Body.String())
	}

	rec = managementQueryGET(t, fixture, "/api/v1/principals?q=Alice&limit=200", fixture.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("principals status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), managementOIDCSubject) || strings.Contains(rec.Body.String(), "oidc_subject") {
		t.Fatalf("principal response leaked OIDC subject: %s", rec.Body.String())
	}
	var principals struct {
		Principals []principalResponse `json:"principals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &principals); err != nil {
		t.Fatal(err)
	}
	if len(principals.Principals) != 1 || principals.Principals[0].ID != "principal-alice" || !principals.Principals[0].Active {
		t.Fatalf("principals = %#v", principals.Principals)
	}

	rec = managementQueryGET(t, fixture, "/api/v1/principals?q=principal-reader", fixture.adminToken)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &principals) != nil || len(principals.Principals) != 1 || principals.Principals[0].ID != fixture.reader.ID {
		t.Fatalf("principal ID search status/body = %d %s", rec.Code, rec.Body.String())
	}
}
