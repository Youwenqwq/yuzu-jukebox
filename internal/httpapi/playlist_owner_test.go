package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// setupPlaylistOwnerEndpoints 三种身份：oidc 创建者、其它 oidc 用户、guest。
func setupPlaylistOwnerEndpoints(t *testing.T) (http.Handler, *store.Store, map[string]string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "playlist-owner.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	authm := auth.NewManager("", st)
	issue := func(id, name, kind string, roles []string) string {
		token := authm.IssueSession(auth.Identity{ID: id, Name: name, Kind: kind, Roles: roles})
		if token == "" {
			t.Fatal("issue session for " + id)
		}
		return token
	}
	tokens := map[string]string{
		"creator": issue("o_creator", "Creator", "oidc", []string{auth.RoleRequester}),
		"other":   issue("o_other", "Other", "oidc", []string{auth.RoleRequester}),
		"guest":   issue("g_guest", "Guest", "guest", []string{auth.RoleRequester}),
		"admin":   issue("p_admin", "Admin", "password", []string{auth.RoleMediaAdmin}),
	}
	s := &Server{st: st, authm: authm, reg: provider.NewRegistry()}
	return s.Handler(), st, tokens
}

func playlistOwnerRequest(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestPlaylistCreateRequiresNonGuest 非 Guest 身份（oidc/password）可建歌单，
// Guest 与匿名被拒。
func TestPlaylistCreateRequiresNonGuest(t *testing.T) {
	h, _, tokens := setupPlaylistOwnerEndpoints(t)

	rec := playlistOwnerRequest(t, h, http.MethodPost, "/api/v1/playlists",
		tokens["creator"], `{"name":"我的歌单"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("oidc create status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = playlistOwnerRequest(t, h, http.MethodPost, "/api/v1/playlists",
		tokens["guest"], `{"name":"Guest 歌单"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("guest create status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = playlistOwnerRequest(t, h, http.MethodPost, "/api/v1/playlists", "", `{"name":"匿名歌单"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous create status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestPlaylistOwnerCanManageOwnPlaylist 创建者（非 admin）可改/删自己的歌单。
func TestPlaylistOwnerCanManageOwnPlaylist(t *testing.T) {
	h, st, tokens := setupPlaylistOwnerEndpoints(t)

	rec := playlistOwnerRequest(t, h, http.MethodPost, "/api/v1/playlists",
		tokens["creator"], `{"name":"我的歌单","description":"d"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Playlist store.Playlist `json:"playlist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	plID := created.Playlist.ID
	if created.Playlist.CreatedBy != "o_creator" {
		t.Fatalf("created_by = %q, want o_creator", created.Playlist.CreatedBy)
	}

	rec = playlistOwnerRequest(t, h, http.MethodPatch, "/api/v1/playlists/"+plID,
		tokens["creator"], `{"name":"改名"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner rename status = %d, body = %s", rec.Code, rec.Body.String())
	}
	pl, err := st.GetPlaylist(context.Background(), plID)
	if err != nil || pl.Name != "改名" {
		t.Fatalf("renamed playlist = %+v (err %v)", pl, err)
	}

	rec = playlistOwnerRequest(t, h, http.MethodDelete, "/api/v1/playlists/"+plID, tokens["creator"], "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner delete status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetPlaylist(context.Background(), plID); err == nil {
		t.Fatal("playlist still exists after owner delete")
	}
}

// TestPlaylistNonOwnerForbidden 非创建者非 admin 改/删他人歌单被拒。
func TestPlaylistNonOwnerForbidden(t *testing.T) {
	h, st, tokens := setupPlaylistOwnerEndpoints(t)
	ctx := context.Background()
	if err := st.CreatePlaylist(ctx, store.Playlist{
		ID: "pl_creator", Name: "创建者的", Description: "", CreatedBy: "o_creator",
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// 其它 oidc 用户（无 admin）→ 403
	rec := playlistOwnerRequest(t, h, http.MethodPatch, "/api/v1/playlists/pl_creator",
		tokens["other"], `{"description":"篡改"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other patch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = playlistOwnerRequest(t, h, http.MethodDelete, "/api/v1/playlists/pl_creator", tokens["other"], "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other delete status = %d, body = %s", rec.Code, rec.Body.String())
	}
	pl, err := st.GetPlaylist(ctx, "pl_creator")
	if err != nil || pl.Description != "" || pl.Name != "创建者的" {
		t.Fatalf("playlist mutated by non-owner = %+v (err %v)", pl, err)
	}

	// admin 仍可管理他人歌单
	rec = playlistOwnerRequest(t, h, http.MethodPatch, "/api/v1/playlists/pl_creator",
		tokens["admin"], `{"description":"管理员备注"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin patch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	pl, _ = st.GetPlaylist(ctx, "pl_creator")
	if pl.Description != "管理员备注" {
		t.Fatalf("admin patch not applied: %+v", pl)
	}
}

// TestPlaylistOwnerItemsAndCoverGated 创建者可加条目/传封面，非创建者被拒。
func TestPlaylistOwnerItemsAndCoverGated(t *testing.T) {
	h, st, tokens := setupPlaylistOwnerEndpoints(t)
	ctx := context.Background()
	if err := st.CreatePlaylist(ctx, store.Playlist{
		ID: "pl_items", Name: "条目", CreatedBy: "o_creator", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// 非创建者加条目 → 403（未到 bound 检查）
	rec := playlistOwnerRequest(t, h, http.MethodPost, "/api/v1/playlists/pl_items/items",
		tokens["other"], `{"track_refs":["local:1"]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other add items status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// 创建者加条目：授权已放行（无 provider 时走到 GetTrack 解析失败，
	// 而非 403 授权拒绝）
	rec = playlistOwnerRequest(t, h, http.MethodPost, "/api/v1/playlists/pl_items/items",
		tokens["creator"], `{"track_refs":["local:1"]}`)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("owner add items forbidden: %s", rec.Body.String())
	}

	// 封面（multipart）由管理授权先行：非创建者传封面 → 403
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "cover.png")
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	mw.Close()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/playlists/pl_items/cover", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tokens["other"])
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other cover status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
