package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
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

func (p *playlistProviderBase) Search(context.Context, string, int, int) ([]provider.Track, error) {
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

func (p *playlistImporterFake) ImportPlaylist(context.Context, string) (string, string, []provider.Track, error) {
	return p.name, "", p.tracks, p.importErr
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

	// 认证走纯内存（nil store）：TestListPlaylistsHidesStoreError 关闭 st 后
	// 认证仍通过，store 错误只影响查询路径。
	authm := auth.NewManager("", nil)
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

func TestListPlaylistsHidesStoreError(t *testing.T) {
	f := setupPlaylistBindingEndpoints(t)
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}

	rec := playlistEndpointRequest(
		t, f, http.MethodGet, "/api/v1/playlists", f.requesterToken, nil,
	)
	body := playlistResponse(t, rec)
	if rec.Code != http.StatusInternalServerError || errCode(t, body) != "internal" {
		t.Fatalf("status = %d, body = %v", rec.Code, body)
	}
	if message := playlistErrorMessage(t, body); message != "internal error" {
		t.Fatalf("internal error message = %q", message)
	}
	if strings.Contains(rec.Body.String(), "database is closed") {
		t.Fatalf("response leaked store error: %s", rec.Body.String())
	}
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
	if failed.Code != http.StatusBadGateway || errCode(t, failedBody) != "provider_error" ||
		playlistErrorMessage(t, failedBody) != "provider request failed" ||
		strings.Contains(failed.Body.String(), f.failing.importErr.Error()) {
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
	if failed.Code != http.StatusBadGateway || errCode(t, failedBody) != "provider_error" ||
		playlistErrorMessage(t, failedBody) != "provider request failed" ||
		strings.Contains(failed.Body.String(), f.importer.importErr.Error()) {
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

type playlistCoverFixture struct {
	handler        http.Handler
	st             *store.Store
	adminToken     string
	requesterToken string
	playlistID     string
	boundID        string
}

func setupPlaylistCoverEndpoints(t *testing.T) playlistCoverFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cover.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	authm := auth.NewManager("", st)
	adminToken := authm.IssueSession(auth.Identity{
		ID: "cover-admin", Name: "Admin", Kind: "password",
		Roles: []string{auth.RoleMediaAdmin},
	})
	requesterToken := authm.IssueSession(auth.Identity{
		ID: "cover-requester", Name: "Requester", Kind: "guest",
		Roles: []string{auth.RoleRequester},
	})
	const playlistID = "pl_cover"
	const boundID = "pl_cover_bound"
	for _, pl := range []store.Playlist{
		{ID: playlistID, Name: "自建歌单", CreatedAt: 1, UpdatedAt: 1},
		{
			ID: boundID, Name: "绑定歌单", CreatedAt: 1, UpdatedAt: 1,
			BoundProvider: "fake", BoundRemoteID: "remote-cover",
		},
	} {
		if err := st.CreatePlaylist(context.Background(), pl); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{st: st, authm: authm}
	s.SetPlaylistCoverDir(filepath.Join(t.TempDir(), "playlist-covers"))
	return playlistCoverFixture{
		handler: s.Handler(), st: st, adminToken: adminToken,
		requesterToken: requesterToken, playlistID: playlistID, boundID: boundID,
	}
}

func uploadPlaylistCover(
	t *testing.T,
	f playlistCoverFixture,
	playlistID, token string,
	data []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "cover.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/playlists/"+playlistID+"/cover", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestPlaylistCoverUploadAndServe(t *testing.T) {
	f := setupPlaylistCoverEndpoints(t)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

	uploaded := uploadPlaylistCover(t, f, f.playlistID, f.adminToken, png)
	if uploaded.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", uploaded.Code, uploaded.Body.String())
	}
	pl, err := f.st.GetPlaylist(context.Background(), f.playlistID)
	if err != nil {
		t.Fatal(err)
	}
	wantURL := "/api/v1/cover/playlist/" + f.playlistID
	if pl.CoverURL != wantURL || !filepath.IsAbs(pl.CoverPath) {
		t.Fatalf("stored cover = (%q, %q)", pl.CoverURL, pl.CoverPath)
	}

	req := httptest.NewRequest(http.MethodGet, wantURL, nil)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("serve status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("content-type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=2592000" {
		t.Fatalf("cache-control = %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), png) {
		t.Fatalf("served bytes = %x, want %x", rec.Body.Bytes(), png)
	}
	oversized := make([]byte, maxPlaylistCoverBytes+1)
	copy(oversized, png)

	for _, tc := range []struct {
		name, playlistID, token string
		data                    []byte
		wantStatus              int
		wantCode                string
	}{
		{"requester forbidden", f.playlistID, f.requesterToken, png, http.StatusForbidden, "forbidden"},
		{"bound conflict", f.boundID, f.adminToken, png, http.StatusConflict, "playlist_bound"},
		{"non image", f.playlistID, f.adminToken, []byte("not an image"), http.StatusBadRequest, "bad_request"},
		{"oversized image", f.playlistID, f.adminToken, oversized, http.StatusBadRequest, "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := uploadPlaylistCover(t, f, tc.playlistID, tc.token, tc.data)
			response := playlistResponse(t, got)
			if got.Code != tc.wantStatus || errCode(t, response) != tc.wantCode {
				t.Fatalf("status = %d, body = %v", got.Code, response)
			}
		})
	}
}

func TestPlaylistCoverDelete(t *testing.T) {
	f := setupPlaylistCoverEndpoints(t)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	uploaded := uploadPlaylistCover(t, f, f.playlistID, f.adminToken, png)
	if uploaded.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", uploaded.Code, uploaded.Body.String())
	}
	before, err := f.st.GetPlaylist(context.Background(), f.playlistID)
	if err != nil {
		t.Fatal(err)
	}

	deleted := playlistEndpointRequest(t, playlistBindingFixture{handler: f.handler},
		http.MethodDelete, "/api/v1/playlists/"+f.playlistID+"/cover", f.adminToken, nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", deleted.Code, deleted.Body.String())
	}
	after, err := f.st.GetPlaylist(context.Background(), f.playlistID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CoverURL != "" || after.CoverPath != "" {
		t.Fatalf("cover not cleared: %+v", after)
	}
	if _, err := os.Stat(before.CoverPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cover file still exists: %v", err)
	}

	served := httptest.NewRecorder()
	f.handler.ServeHTTP(served, httptest.NewRequest(http.MethodGet,
		"/api/v1/cover/playlist/"+f.playlistID, nil))
	if served.Code != http.StatusNotFound {
		t.Fatalf("serve after delete status = %d", served.Code)
	}

	bound := playlistEndpointRequest(t, playlistBindingFixture{handler: f.handler},
		http.MethodDelete, "/api/v1/playlists/"+f.boundID+"/cover", f.adminToken, nil)
	boundBody := playlistResponse(t, bound)
	if bound.Code != http.StatusConflict || errCode(t, boundBody) != "playlist_bound" {
		t.Fatalf("bound delete status = %d, body = %v", bound.Code, boundBody)
	}
}
