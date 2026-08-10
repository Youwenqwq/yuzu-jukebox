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
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func setupPlaylistMetaEndpoint(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "playlist-meta.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.CreatePlaylist(ctx, store.Playlist{
		ID: "pl_meta", Name: "旧名", Description: "旧描述", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePlaylist(ctx, store.Playlist{
		ID: "pl_bound", Name: "绑定名", Description: "远程描述",
		BoundProvider: "ncm", BoundRemoteID: "remote-1", CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}

	authm := auth.NewManager("", st)
	adminTok := authm.IssueSession(auth.Identity{ID: "u_admin", Name: "admin", Kind: "password",
		Roles: []string{auth.RoleMediaAdmin}})
	requesterTok := authm.IssueSession(auth.Identity{ID: "u_req", Name: "req", Kind: "guest",
		Roles: []string{auth.RoleRequester}})
	s := &Server{st: st, authm: authm}
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/v1/playlists/{id}", s.updatePlaylistMeta)
	mux.HandleFunc("GET /api/v1/playlists/{id}", s.getPlaylist)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_ = requesterTok
	return srv, st, adminTok
}

func patchPlaylistMeta(t *testing.T, srv *httptest.Server, token, plID, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("PATCH", srv.URL+"/api/v1/playlists/"+plID, strings.NewReader(body))
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

func TestUpdatePlaylistMeta(t *testing.T) {
	srv, st, adminTok := setupPlaylistMetaEndpoint(t)

	// 改名 + 改描述（部分更新语义：同时改）
	code, body := patchPlaylistMeta(t, srv, adminTok, "pl_meta", `{"name":"新名","description":"新描述"}`)
	if code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", code, body)
	}
	pl, err := st.GetPlaylist(context.Background(), "pl_meta")
	if err != nil {
		t.Fatal(err)
	}
	if pl.Name != "新名" || pl.Description != "新描述" {
		t.Fatalf("updated playlist = %+v", pl)
	}

	// 只 pin：name/description 不变
	code, _ = patchPlaylistMeta(t, srv, adminTok, "pl_meta", `{"pinned":true}`)
	if code != http.StatusOK {
		t.Fatalf("pin status = %d", code)
	}
	pl, _ = st.GetPlaylist(context.Background(), "pl_meta")
	if !pl.Pinned || pl.Name != "新名" || pl.Description != "新描述" {
		t.Fatalf("pinned playlist = %+v", pl)
	}

	// description 显式清空
	code, _ = patchPlaylistMeta(t, srv, adminTok, "pl_meta", `{"description":""}`)
	if code != http.StatusOK {
		t.Fatalf("clear description status = %d", code)
	}
	pl, _ = st.GetPlaylist(context.Background(), "pl_meta")
	if pl.Description != "" {
		t.Fatalf("description not cleared: %+v", pl)
	}

	// PATCH 响应体携带更新后的 playlist
	code, body = patchPlaylistMeta(t, srv, adminTok, "pl_meta", `{"name":"响应名"}`)
	if code != http.StatusOK {
		t.Fatalf("response update status = %d", code)
	}
	var resp struct {
		Playlist store.Playlist `json:"playlist"`
	}
	raw, _ := json.Marshal(body)
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Playlist.Name != "响应名" {
		t.Fatalf("response playlist = %+v (err %v), want updated name", resp.Playlist, err)
	}
}

func TestUpdatePlaylistMetaValidation(t *testing.T) {
	srv, _, adminTok := setupPlaylistMetaEndpoint(t)

	// 空 body → 400
	code, _ := patchPlaylistMeta(t, srv, adminTok, "pl_meta", `{}`)
	if code != http.StatusBadRequest {
		t.Fatalf("empty body status = %d, want 400", code)
	}

	// 空名字 → 400
	code, _ = patchPlaylistMeta(t, srv, adminTok, "pl_meta", `{"name":"   "}`)
	if code != http.StatusBadRequest {
		t.Fatalf("blank name status = %d, want 400", code)
	}

	// 非法 JSON → 400
	code, _ = patchPlaylistMeta(t, srv, adminTok, "pl_meta", `{`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid json status = %d, want 400", code)
	}

	// 不存在的歌单 → 404
	code, _ = patchPlaylistMeta(t, srv, adminTok, "pl_nope", `{"name":"x"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown playlist status = %d, want 404", code)
	}
}

func TestUpdatePlaylistMetaBoundNameRejected(t *testing.T) {
	srv, st, adminTok := setupPlaylistMetaEndpoint(t)

	// 绑定歌单改名 → 409 playlist_bound
	code, body := patchPlaylistMeta(t, srv, adminTok, "pl_bound", `{"name":"改名"}`)
	if code != http.StatusConflict {
		t.Fatalf("bound rename status = %d, body = %s", code, body)
	}
	if got := errCode(t, body); got != "playlist_bound" {
		t.Fatalf("bound rename error = %q, want playlist_bound", got)
	}

	// 绑定歌单改描述/pin（本地状态）→ 允许
	code, _ = patchPlaylistMeta(t, srv, adminTok, "pl_bound", `{"description":"本地备注","pinned":true}`)
	if code != http.StatusOK {
		t.Fatalf("bound description status = %d", code)
	}
	pl, err := st.GetPlaylist(context.Background(), "pl_bound")
	if err != nil {
		t.Fatal(err)
	}
	if pl.Description != "本地备注" || !pl.Pinned || pl.Name != "绑定名" {
		t.Fatalf("bound playlist local state = %+v", pl)
	}
}

func TestUpdatePlaylistMetaRequiresAdmin(t *testing.T) {
	srv, _, _ := setupPlaylistMetaEndpoint(t)

	// 无 token 请求 → 401
	req, err := http.NewRequest("PATCH", srv.URL+"/api/v1/playlists/pl_meta", strings.NewReader(`{"name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", resp.StatusCode)
	}
}

// TestListPlaylistsPinnedFirst 置顶歌单排在未置顶之前（Library 排序）。
func TestListPlaylistsPinnedFirst(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pin-order.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, p := range []store.Playlist{
		{ID: "pl_a", Name: "A", CreatedAt: 1, UpdatedAt: 1},
		{ID: "pl_b", Name: "B", Pinned: true, CreatedAt: 2, UpdatedAt: 2},
		{ID: "pl_c", Name: "C", CreatedAt: 3, UpdatedAt: 3},
	} {
		p.CreatedBy = "u"
		if err := st.CreatePlaylist(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	all, err := st.ListPlaylists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != "pl_b" || !all[0].Pinned {
		t.Fatalf("playlists = %#v, want pl_b pinned first", all)
	}
	if all[1].ID != "pl_a" || all[2].ID != "pl_c" {
		t.Fatalf("playlists order = %#v, want pl_a then pl_c", all)
	}
}
