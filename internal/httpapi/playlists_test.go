package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
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
