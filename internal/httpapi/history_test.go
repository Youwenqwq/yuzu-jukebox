package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
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
		if err := f.st.AddPlayHistory(ctx, row.roomID, row.ref, row.ref, row.requester, row.startedAt, row.startedAt+1, "finished"); err != nil {
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
	}
	for _, row := range rows {
		if err := f.st.AddPlayHistory(ctx, row.roomID, row.ref, row.title, row.requester, row.startedAt, row.startedAt+1, "finished"); err != nil {
			t.Fatal(err)
		}
	}

	rec := historyEndpointRequest(t, f, "/api/v1/stats/hot?days=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("hot status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Tracks []store.HotTrack `json:"tracks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tracks) != 2 {
		t.Fatalf("hot tracks = %#v, want 2 entries", body.Tracks)
	}
	first := body.Tracks[0]
	if first.TrackRef != "ncm:a" || first.Title != "A new" || first.PlayCount != 2 || first.LastPlayedAt != 300 {
		t.Fatalf("hot first track = %#v, want aggregated ncm:a", first)
	}

	rec = historyEndpointRequest(t, f, "/api/v1/stats/hot?days=-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative days status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
