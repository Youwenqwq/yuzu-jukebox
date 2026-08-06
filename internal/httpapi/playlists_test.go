package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func setupMoveEndpoint(t *testing.T, n int) (*httptest.Server, *store.Store, string, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreatePlaylist(ctx, store.Playlist{ID: "pl_t", Name: "t", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	items := make([]store.PlaylistItem, n)
	for i := range items {
		items[i] = store.PlaylistItem{TrackRef: fmt.Sprintf("t%d", i+1), Title: fmt.Sprintf("t%d", i+1)}
	}
	if err := st.AppendPlaylistItems(ctx, "pl_t", items); err != nil {
		t.Fatal(err)
	}

	authm := auth.NewManager("", st)
	adminTok := authm.IssueSession(auth.Identity{ID: "u_admin", Name: "admin", Kind: "password",
		Roles: []string{auth.RoleMediaAdmin}})
	s := &Server{st: st, authm: authm}
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/v1/playlists/{id}/items/{ord}", s.movePlaylistItem)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st, "pl_t", adminTok
}

func patchItem(t *testing.T, srv *httptest.Server, token, plID, ord, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("PATCH",
		fmt.Sprintf("%s/api/v1/playlists/%s/items/%s", srv.URL, plID, ord),
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func errCode(t *testing.T, body map[string]any) string {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %v", body)
	}
	code, _ := e["code"].(string)
	return code
}

func TestMovePlaylistItemEndpoint(t *testing.T) {
	srv, st, pl, tok := setupMoveEndpoint(t, 5)

	status, body := patchItem(t, srv, tok, pl, "2", `{"to_ord": 4}`)
	if status != http.StatusOK {
		t.Fatalf("status %d body %v", status, body)
	}
	if body["moved"] != 2.0 || body["to_ord"] != 4.0 {
		t.Fatalf("unexpected response %v", body)
	}
	items, err := st.PlaylistItems(context.Background(), pl, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"t1", "t3", "t4", "t2", "t5"}
	for i, it := range items {
		if it.Ord != i+1 || it.TrackRef != want[i] {
			t.Fatalf("position %d: ord %d ref %s, want ord %d ref %s", i, it.Ord, it.TrackRef, i+1, want[i])
		}
	}
}

func TestMovePlaylistItemEndpointClamp(t *testing.T) {
	srv, _, pl, tok := setupMoveEndpoint(t, 3)

	status, body := patchItem(t, srv, tok, pl, "1", `{"to_ord": 99}`)
	if status != http.StatusOK || body["to_ord"] != 3.0 {
		t.Fatalf("clamp high: status %d body %v", status, body)
	}
	status, body = patchItem(t, srv, tok, pl, "2", `{"to_ord": -5}`)
	if status != http.StatusOK || body["to_ord"] != 1.0 {
		t.Fatalf("clamp low: status %d body %v", status, body)
	}
}

func TestMovePlaylistItemEndpointNotFound(t *testing.T) {
	srv, _, pl, tok := setupMoveEndpoint(t, 3)

	// 歌单不存在
	status, body := patchItem(t, srv, tok, "pl_nope", "1", `{"to_ord": 1}`)
	if status != http.StatusNotFound || errCode(t, body) != "not_found" {
		t.Fatalf("missing playlist: status %d body %v", status, body)
	}
	// ord 越界
	for _, ord := range []string{"0", "4", "99"} {
		status, body = patchItem(t, srv, tok, pl, ord, `{"to_ord": 1}`)
		if status != http.StatusNotFound || errCode(t, body) != "not_found" {
			t.Fatalf("ord %s: status %d body %v", ord, status, body)
		}
	}
}

func TestMovePlaylistItemEndpointBadRequest(t *testing.T) {
	srv, _, pl, tok := setupMoveEndpoint(t, 3)

	cases := []struct{ ord, body string }{
		{"abc", `{"to_ord": 1}`}, // ord 非整数
		{"1", `{"to_ord": "x"}`}, // to_ord 非整数
		{"1", `{"to_ord": 1.5}`}, // to_ord 非整数
		{"1", `{}`},              // to_ord 缺失
		{"1", `not-json`},        // 非法 JSON
	}
	for _, c := range cases {
		status, body := patchItem(t, srv, tok, pl, c.ord, c.body)
		if status != http.StatusBadRequest || errCode(t, body) != "bad_request" {
			t.Fatalf("ord %s body %s: status %d body %v", c.ord, c.body, status, body)
		}
	}
}

func TestMovePlaylistItemEndpointRole(t *testing.T) {
	srv, _, pl, _ := setupMoveEndpoint(t, 3)

	// 无 token → 401；requester 角色 → 403
	status, _ := patchItem(t, srv, "", pl, "1", `{"to_ord": 2}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("no token: status %d", status)
	}
}

type playlistProviderBase struct {
	id string
}

func (p *playlistProviderBase) ID() string { return p.id }

func (p *playlistProviderBase) Search(context.Context, string) ([]provider.Track, error) {
	return nil, nil
}

func (p *playlistProviderBase) GetTrack(_ context.Context, ref provider.TrackRef) (provider.Track, error) {
	return provider.Track{Ref: ref, Title: ref.String()}, nil
}

func (p *playlistProviderBase) Resolve(context.Context, provider.TrackRef) (provider.StreamLocator, error) {
	return provider.StreamLocator{}, nil
}

type playlistImporterFake struct {
	playlistProviderBase
	name      string
	tracks    []provider.Track
	importErr error
}

func (p *playlistImporterFake) ImportPlaylist(context.Context, string) (string, []provider.Track, error) {
	return p.name, p.tracks, p.importErr
}

type playlistBindingFixture struct {
	handler        http.Handler
	st             *store.Store
	adminToken     string
	requesterToken string
	importer       *playlistImporterFake
	failing        *playlistImporterFake
}

func setupPlaylistBindingEndpoints(t *testing.T) playlistBindingFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	authm := auth.NewManager("", st)
	adminToken := authm.IssueSession(auth.Identity{
		ID: "playlist-admin", Name: "Admin", Kind: "password",
		Roles: []string{auth.RoleMediaAdmin},
	})
	requesterToken := authm.IssueSession(auth.Identity{
		ID: "playlist-requester", Name: "Requester", Kind: "guest",
		Roles: []string{auth.RoleRequester},
	})

	reg := provider.NewRegistry()
	importer := &playlistImporterFake{
		playlistProviderBase: playlistProviderBase{id: "playlist-fake"},
		name:                 "远端歌单",
		tracks: []provider.Track{
			{Ref: provider.NewRef("playlist-fake", "one"), Title: "第一首", Artist: "歌手甲", DurationMs: 1000},
			{Ref: provider.NewRef("playlist-fake", "two"), Title: "第二首", Artist: "歌手乙", DurationMs: 2000},
		},
	}
	failing := &playlistImporterFake{
		playlistProviderBase: playlistProviderBase{id: "playlist-failing"},
		importErr:            errors.New("远端暂不可用"),
	}
	reg.Register(importer)
	reg.Register(failing)
	reg.Register(&playlistProviderBase{id: "playlist-basic"})

	s := &Server{st: st, authm: authm, reg: reg}
	return playlistBindingFixture{
		handler: s.Handler(), st: st, adminToken: adminToken,
		requesterToken: requesterToken, importer: importer, failing: failing,
	}
}

func playlistEndpointRequest(
	t *testing.T,
	f playlistBindingFixture,
	method, path, token string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func playlistResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

func playlistErrorMessage(t *testing.T, body map[string]any) string {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %v", body)
	}
	message, _ := e["message"].(string)
	return message
}

func bindTestPlaylist(t *testing.T, f playlistBindingFixture, remoteID string) store.Playlist {
	t.Helper()
	rec := playlistEndpointRequest(t, f, http.MethodPost, "/api/v1/playlists/bind",
		f.adminToken, map[string]string{"provider": f.importer.ID(), "playlist_id": remoteID})
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status = %d, body = %s", rec.Code, rec.Body.String())
	}
	pl, found, err := f.st.GetPlaylistByBinding(context.Background(), f.importer.ID(), remoteID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("bound playlist not found")
	}
	return pl
}

func TestBindPlaylistEndpoint(t *testing.T) {
	f := setupPlaylistBindingEndpoints(t)
	rec := playlistEndpointRequest(t, f, http.MethodPost, "/api/v1/playlists/bind",
		f.adminToken, map[string]string{"provider": f.importer.ID(), "playlist_id": "remote-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := playlistResponse(t, rec)
	if body["synced"] != 2.0 {
		t.Fatalf("synced = %v, want 2", body["synced"])
	}
	responsePlaylist, ok := body["playlist"].(map[string]any)
	if !ok {
		t.Fatalf("playlist response = %v", body["playlist"])
	}
	if responsePlaylist["name"] != f.importer.name ||
		responsePlaylist["bound_provider"] != f.importer.ID() ||
		responsePlaylist["bound_remote_id"] != "remote-1" {
		t.Fatalf("playlist response = %v", responsePlaylist)
	}

	pl, found, err := f.st.GetPlaylistByBinding(context.Background(), f.importer.ID(), "remote-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || pl.Name != f.importer.name || pl.BoundProvider != f.importer.ID() ||
		pl.BoundRemoteID != "remote-1" || pl.TrackCount != 2 {
		t.Fatalf("stored playlist = %+v, found = %v", pl, found)
	}
	items, err := f.st.PlaylistItems(context.Background(), pl.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].TrackRef != "playlist-fake:one" ||
		items[1].TrackRef != "playlist-fake:two" {
		t.Fatalf("synced items = %+v", items)
	}

	duplicate := playlistEndpointRequest(t, f, http.MethodPost, "/api/v1/playlists/bind",
		f.adminToken, map[string]string{"provider": f.importer.ID(), "playlist_id": "remote-1"})
	duplicateBody := playlistResponse(t, duplicate)
	if duplicate.Code != http.StatusConflict || errCode(t, duplicateBody) != "already_bound" ||
		!strings.Contains(playlistErrorMessage(t, duplicateBody), pl.ID) {
		t.Fatalf("duplicate status = %d, body = %v", duplicate.Code, duplicateBody)
	}

	unsupported := playlistEndpointRequest(t, f, http.MethodPost, "/api/v1/playlists/bind",
		f.adminToken, map[string]string{"provider": "playlist-basic", "playlist_id": "remote-2"})
	unsupportedBody := playlistResponse(t, unsupported)
	if unsupported.Code != http.StatusBadRequest || errCode(t, unsupportedBody) != "bad_request" {
		t.Fatalf("unsupported status = %d, body = %v", unsupported.Code, unsupportedBody)
	}

	failed := playlistEndpointRequest(t, f, http.MethodPost, "/api/v1/playlists/bind",
		f.adminToken, map[string]string{"provider": f.failing.ID(), "playlist_id": "remote-bad"})
	failedBody := playlistResponse(t, failed)
	if failed.Code != http.StatusBadGateway || errCode(t, failedBody) != "provider_error" {
		t.Fatalf("failed bind status = %d, body = %v", failed.Code, failedBody)
	}
	_, found, err = f.st.GetPlaylistByBinding(context.Background(), f.failing.ID(), "remote-bad")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("failed first sync left a bound playlist")
	}
	playlists, err := f.st.ListPlaylists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(playlists) != 1 || playlists[0].ID != pl.ID {
		t.Fatalf("playlists after failed bind = %+v", playlists)
	}
}

func TestSyncPlaylistEndpoint(t *testing.T) {
	f := setupPlaylistBindingEndpoints(t)
	pl := bindTestPlaylist(t, f, "sync-me")
	f.importer.tracks = []provider.Track{
		{Ref: provider.NewRef("playlist-fake", "new-1"), Title: "新一"},
		{Ref: provider.NewRef("playlist-fake", "new-2"), Title: "新二"},
		{Ref: provider.NewRef("playlist-fake", "new-3"), Title: "新三"},
	}

	rec := playlistEndpointRequest(t, f, http.MethodPost,
		"/api/v1/playlists/"+pl.ID+"/sync", f.adminToken, nil)
	body := playlistResponse(t, rec)
	if rec.Code != http.StatusOK || body["synced"] != 3.0 {
		t.Fatalf("sync status = %d, body = %v", rec.Code, body)
	}
	items, err := f.st.PlaylistItems(context.Background(), pl.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].TrackRef != "playlist-fake:new-1" ||
		items[2].TrackRef != "playlist-fake:new-3" {
		t.Fatalf("items after sync = %+v", items)
	}

	f.importer.importErr = errors.New("同步失败")
	failed := playlistEndpointRequest(t, f, http.MethodPost,
		"/api/v1/playlists/"+pl.ID+"/sync", f.adminToken, nil)
	failedBody := playlistResponse(t, failed)
	if failed.Code != http.StatusBadGateway || errCode(t, failedBody) != "provider_error" {
		t.Fatalf("failed sync status = %d, body = %v", failed.Code, failedBody)
	}

	unbound := store.Playlist{
		ID: "pl_unbound", Name: "普通歌单", CreatedBy: "playlist-admin",
		CreatedAt: 1, UpdatedAt: 1,
	}
	if err := f.st.CreatePlaylist(context.Background(), unbound); err != nil {
		t.Fatal(err)
	}
	normal := playlistEndpointRequest(t, f, http.MethodPost,
		"/api/v1/playlists/"+unbound.ID+"/sync", f.adminToken, nil)
	normalBody := playlistResponse(t, normal)
	if normal.Code != http.StatusBadRequest || errCode(t, normalBody) != "bad_request" {
		t.Fatalf("unbound sync status = %d, body = %v", normal.Code, normalBody)
	}
}

func TestDetachPlaylistEndpointAllowsMutations(t *testing.T) {
	f := setupPlaylistBindingEndpoints(t)
	pl := bindTestPlaylist(t, f, "detach-me")

	rec := playlistEndpointRequest(t, f, http.MethodPost,
		"/api/v1/playlists/"+pl.ID+"/detach", f.adminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detach status = %d, body = %s", rec.Code, rec.Body.String())
	}
	detached, err := f.st.GetPlaylist(context.Background(), pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detached.BoundProvider != "" || detached.BoundRemoteID != "" || detached.TrackCount != 2 {
		t.Fatalf("detached playlist = %+v", detached)
	}

	added := playlistEndpointRequest(t, f, http.MethodPost,
		"/api/v1/playlists/"+pl.ID+"/items", f.adminToken,
		map[string]any{"track_refs": []string{"playlist-fake:after-detach"}})
	if added.Code != http.StatusOK {
		t.Fatalf("add after detach status = %d, body = %s", added.Code, added.Body.String())
	}
	items, err := f.st.PlaylistItems(context.Background(), pl.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[2].TrackRef != "playlist-fake:after-detach" {
		t.Fatalf("items after detach mutation = %+v", items)
	}

	again := playlistEndpointRequest(t, f, http.MethodPost,
		"/api/v1/playlists/"+pl.ID+"/detach", f.adminToken, nil)
	againBody := playlistResponse(t, again)
	if again.Code != http.StatusBadRequest || errCode(t, againBody) != "bad_request" {
		t.Fatalf("second detach status = %d, body = %v", again.Code, againBody)
	}
}

func TestBoundPlaylistRejectsItemMutations(t *testing.T) {
	f := setupPlaylistBindingEndpoints(t)
	pl := bindTestPlaylist(t, f, "read-only")
	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{
			name: "add", method: http.MethodPost,
			path: "/api/v1/playlists/" + pl.ID + "/items",
			body: map[string]any{"track_refs": []string{"playlist-fake:new"}},
		},
		{
			name: "delete", method: http.MethodDelete,
			path: "/api/v1/playlists/" + pl.ID + "/items/1",
		},
		{
			name: "move", method: http.MethodPatch,
			path: "/api/v1/playlists/" + pl.ID + "/items/1",
			body: map[string]int{"to_ord": 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := playlistEndpointRequest(t, f, tc.method, tc.path, f.adminToken, tc.body)
			body := playlistResponse(t, rec)
			if rec.Code != http.StatusConflict || errCode(t, body) != "playlist_bound" ||
				playlistErrorMessage(t, body) != "playlist is provider-bound; detach first" {
				t.Fatalf("status = %d, body = %v", rec.Code, body)
			}
		})
	}
	items, err := f.st.PlaylistItems(context.Background(), pl.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].TrackRef != "playlist-fake:one" ||
		items[1].TrackRef != "playlist-fake:two" {
		t.Fatalf("guarded items changed = %+v", items)
	}
}

func TestPlaylistBindingEndpointsRequireMediaAdmin(t *testing.T) {
	f := setupPlaylistBindingEndpoints(t)
	cases := []struct {
		path string
		body any
	}{
		{
			path: "/api/v1/playlists/bind",
			body: map[string]string{"provider": f.importer.ID(), "playlist_id": "auth-check"},
		},
		{path: "/api/v1/playlists/pl_missing/sync"},
		{path: "/api/v1/playlists/pl_missing/detach"},
	}
	for _, tc := range cases {
		for _, authCase := range []struct {
			name   string
			token  string
			status int
		}{
			{name: "anonymous", status: http.StatusUnauthorized},
			{name: "requester", token: f.requesterToken, status: http.StatusForbidden},
		} {
			t.Run(tc.path+"/"+authCase.name, func(t *testing.T) {
				rec := playlistEndpointRequest(t, f, http.MethodPost, tc.path, authCase.token, tc.body)
				if rec.Code != authCase.status {
					t.Fatalf("status = %d, want %d, body = %s",
						rec.Code, authCase.status, rec.Body.String())
				}
			})
		}
	}
}
