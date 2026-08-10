package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"

	"golang.org/x/crypto/bcrypt"
)

type historyEndpointFixture struct {
	handler http.Handler
	st      *store.Store
	token   string
}

func setupHistoryEndpoints(t *testing.T) historyEndpointFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "history-http.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	authm := auth.NewManager("", st)
	token := authm.IssueSession(auth.Identity{
		ID: "alice", Name: "Alice", Kind: "guest", Roles: []string{auth.RoleRequester},
	})
	if token == "" {
		t.Fatal("issue requester session")
	}
	reg := provider.NewRegistry()
	controls := control.NewService(nil, reg, control.NewAuthorizer(st))
	s := &Server{
		st: st, authm: authm, reg: reg, controls: controls,
		ws: wsapi.NewServer(authm, auth.NewPlayerRegistry(st), controls, st),
	}
	return historyEndpointFixture{handler: s.Handler(), st: st, token: token}
}

func historyEndpointRequest(t *testing.T, f historyEndpointFixture, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestRequesterHistoryEndpointOnlyReturnsCallerRows(t *testing.T) {
	f := setupHistoryEndpoints(t)
	ctx := context.Background()
	for _, row := range []struct {
		roomID, ref, requester string
		startedAt              int64
	}{
		{"room-1", "ncm:old", "alice", 100},
		{"room-2", "ncm:other", "bob", 300},
		{"room-2", "ncm:new", "alice", 500},
	} {
		if err := f.st.AddPlayHistory(ctx, row.roomID, row.ref, row.ref, "artist", row.requester, row.startedAt, row.startedAt+1, "finished"); err != nil {
			t.Fatal(err)
		}
	}

	rec := historyEndpointRequest(t, f, "/api/v1/history?requester=me")
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		History []store.PlayHistoryRow `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.History) != 2 || body.History[0].TrackRef != "ncm:new" || body.History[1].TrackRef != "ncm:old" {
		t.Fatalf("history = %#v, want caller rows newest first", body.History)
	}
	for _, row := range body.History {
		if row.RequestedBy != "alice" {
			t.Fatalf("history leaked requester %q: %#v", row.RequestedBy, body.History)
		}
	}

	rec = historyEndpointRequest(t, f, "/api/v1/history?requester=other")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("other requester status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var errorBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errorBody); err != nil {
		t.Fatal(err)
	}
	if errorBody.Error.Code != "bad_request" {
		t.Fatalf("other requester error = %#v, want bad_request", errorBody)
	}
}

func TestHotTracksEndpoint(t *testing.T) {
	f := setupHistoryEndpoints(t)
	ctx := context.Background()
	rows := []struct {
		roomID, ref, title, requester string
		startedAt                     int64
	}{
		{"room-1", "ncm:a", "A old", "alice", 100},
		{"room-2", "ncm:b", "B", "bob", 200},
		{"room-2", "ncm:a", "A new", "bob", 300},
		{"room-1", "ncm:c", "C", "alice", 400},
		{"room-1", "ncm:d", "D", "alice", 500},
		{"room-1", "ncm:e", "E", "alice", 600},
		{"room-1", "ncm:f", "F", "alice", 700},
		{"room-1", "ncm:g", "G", "alice", 800},
	}
	for _, row := range rows {
		if err := f.st.AddPlayHistory(ctx, row.roomID, row.ref, row.title, "artist", row.requester, row.startedAt, row.startedAt+1, "finished"); err != nil {
			t.Fatal(err)
		}
	}

	rec := historyEndpointRequest(t, f, "/api/v1/stats/hot?days=0&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("hot status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var firstPage struct {
		Tracks []store.HotTrack `json:"tracks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Tracks) != 2 {
		t.Fatalf("hot tracks = %#v, want 2 entries", firstPage.Tracks)
	}
	first := firstPage.Tracks[0]
	if first.TrackRef != "ncm:a" || first.Title != "A new" || first.PlayCount != 2 || first.LastPlayedAt != 300 {
		t.Fatalf("hot first track = %#v, want aggregated ncm:a", first)
	}

	rec = historyEndpointRequest(t, f, "/api/v1/stats/hot?days=0&limit=2&offset=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("offset hot status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var offsetPage struct {
		Tracks []store.HotTrack `json:"tracks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &offsetPage); err != nil {
		t.Fatal(err)
	}
	if len(offsetPage.Tracks) != 2 ||
		offsetPage.Tracks[0].TrackRef != "ncm:c" ||
		offsetPage.Tracks[1].TrackRef != "ncm:b" {
		t.Fatalf("offset hot tracks = %#v, want ncm:c then ncm:b", offsetPage.Tracks)
	}
	if offsetPage.Tracks[0].TrackRef == firstPage.Tracks[0].TrackRef {
		t.Fatalf("offset page did not advance: first = %#v, offset = %#v", firstPage.Tracks, offsetPage.Tracks)
	}

	rec = historyEndpointRequest(t, f, "/api/v1/stats/hot?days=-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative days status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = historyEndpointRequest(t, f, "/api/v1/stats/hot?days=0&offset=-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative offset status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

type roomScopedReadFixture struct {
	handler http.Handler
	tokens  map[string]string
}

func setupRoomScopedReadEndpoints(t *testing.T) roomScopedReadFixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "room-scoped-read.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = st.Close()
	})

	authm := auth.NewManager("", st)
	reg := provider.NewRegistry()
	roomCache := cache.New(filepath.Join(root, "cache"), 1<<20, 0, st, reg)
	rooms := room.NewManager(ctx, st, authm, roomCache, reg, nil)
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("room-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	roomRows := []store.Room{
		{
			ID: "open-room", Name: "Open", AccessMode: string(room.AccessModeOpen),
			CodePeriodSeconds: room.DefaultCodePeriodSeconds, CreatedAt: 1,
		},
		{
			ID: "protected-room", Name: "Protected", PasswordHash: string(passwordHash),
			AccessMode: string(room.AccessModeStaticPassword), CodePeriodSeconds: room.DefaultCodePeriodSeconds,
			TrustedRoles: []string{"staff"}, CreatedAt: 2,
		},
	}
	for _, row := range roomRows {
		if err := st.CreateRoom(ctx, row); err != nil {
			t.Fatalf("create %s: %v", row.ID, err)
		}
		rooms.Spawn(row)
		if err := st.AddPlayHistory(ctx, row.ID, "test:track", "Track", "Artist", "requester", 10, 20, "finished"); err != nil {
			t.Fatalf("seed %s history: %v", row.ID, err)
		}
	}

	identities := map[string]auth.Identity{
		"guest": {
			ID: "guest", Name: "Guest", Kind: "guest", Roles: []string{auth.RoleListener},
		},
		"controller": {
			ID: "controller", Name: "Controller", Kind: "oidc", Roles: []string{auth.RoleListener},
		},
		"trusted": {
			ID: "trusted", Name: "Trusted", Kind: "oidc", Roles: []string{auth.RoleListener, "staff"},
		},
	}
	tokens := make(map[string]string, len(identities))
	for name, identity := range identities {
		tokens[name] = authm.IssueSession(identity)
		if tokens[name] == "" {
			t.Fatalf("issue %s session", name)
		}
	}
	if err := st.GrantRoomGrant(ctx, "protected-room", identities["controller"].ID, control.CapabilityController); err != nil {
		t.Fatalf("grant protected-room controller: %v", err)
	}

	controls := control.NewService(rooms, reg, control.NewAuthorizer(st))
	s := &Server{
		st: st, authm: authm, rooms: rooms, reg: reg, cache: roomCache, controls: controls,
		ws: wsapi.NewServer(authm, auth.NewPlayerRegistry(st), controls, st),
	}
	return roomScopedReadFixture{handler: s.Handler(), tokens: tokens}
}

func TestRoomHistoryAndStatsRequireRoomAdmission(t *testing.T) {
	f := setupRoomScopedReadEndpoints(t)
	cases := []struct {
		name, roomID, tokenName string
		wantStatus              int
	}{
		{name: "open guest", roomID: "open-room", tokenName: "guest", wantStatus: http.StatusOK},
		{name: "protected guest", roomID: "protected-room", tokenName: "guest", wantStatus: http.StatusForbidden},
		{name: "protected controller", roomID: "protected-room", tokenName: "controller", wantStatus: http.StatusOK},
		{name: "protected trusted role", roomID: "protected-room", tokenName: "trusted", wantStatus: http.StatusOK},
	}
	for _, tc := range cases {
		for _, endpoint := range []string{"history", "stats"} {
			t.Run(tc.name+" "+endpoint, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/rooms/"+tc.roomID+"/"+endpoint, nil)
				req.Header.Set("Authorization", "Bearer "+f.tokens[tc.tokenName])
				rec := httptest.NewRecorder()
				f.handler.ServeHTTP(rec, req)
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
				}
				if tc.wantStatus == http.StatusForbidden {
					var body struct {
						Error struct {
							Code string `json:"code"`
						} `json:"error"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if body.Error.Code != "forbidden" {
						t.Fatalf("error code = %q, want forbidden", body.Error.Code)
					}
				}
			})
		}
	}
}
