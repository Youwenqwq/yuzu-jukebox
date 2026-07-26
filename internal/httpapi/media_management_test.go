package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider/local"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
)

type mediaEndpointFixture struct {
	srv       *httptest.Server
	st        *store.Store
	local     *local.Provider
	mediaDir  string
	cacheDir  string
	adminTok  string
	readerTok string
}

func setupMediaEndpoints(t *testing.T) mediaEndpointFixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mediaDir := filepath.Join(root, "media")
	cacheDir := filepath.Join(root, "cache")
	for _, dir := range []string{mediaDir, cacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lp := local.New(mediaDir, st)
	reg := provider.NewRegistry()
	reg.Register(lp)
	c := cache.New(cacheDir, 1<<20, st, reg)
	authm := auth.NewManager("", st)
	adminTok := authm.IssueSession(auth.Identity{ID: "u_admin", Name: "admin", Roles: []string{auth.RoleMediaAdmin}})
	readerTok := authm.IssueSession(auth.Identity{ID: "u_reader", Name: "reader", Roles: []string{auth.RoleRequester}})
	s := &Server{st: st, authm: authm, reg: reg, local: lp, cache: c, ws: wsapi.NewServer(authm, nil, reg)}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return mediaEndpointFixture{srv: srv, st: st, local: lp, mediaDir: mediaDir, cacheDir: cacheDir, adminTok: adminTok, readerTok: readerTok}
}

func mediaEndpointRequest(t *testing.T, f mediaEndpointFixture, method, path, token string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestListMediaEndpointRoleAndResponse(t *testing.T) {
	f := setupMediaEndpoints(t)
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{name: "unauthenticated", want: http.StatusUnauthorized},
		{name: "wrong role", token: f.readerTok, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := mediaEndpointRequest(t, f, http.MethodGet, "/api/v1/media", tc.token)
			if status != tc.want {
				t.Fatalf("status %d, want %d", status, tc.want)
			}
		})
	}

	status, body := mediaEndpointRequest(t, f, http.MethodGet, "/api/v1/media", f.adminTok)
	if status != http.StatusOK {
		t.Fatalf("empty list status %d body %v", status, body)
	}
	media, ok := body["media"].([]any)
	if !ok || len(media) != 0 {
		t.Fatalf("empty media = %#v, want []", body["media"])
	}

	ctx := context.Background()
	for _, file := range []store.MediaFile{
		{ID: "older", Filename: "older.mp3", Title: "Older", Artist: "A", DurationMs: 10, SizeBytes: 20, UploadedBy: "one", CreatedAt: 100},
		{ID: "newer", Filename: "newer.mp3", Title: "Newer", Artist: "B", DurationMs: 30, SizeBytes: 40, UploadedBy: "two", CreatedAt: 200},
	} {
		if err := f.st.AddMediaFile(ctx, file); err != nil {
			t.Fatal(err)
		}
	}
	status, body = mediaEndpointRequest(t, f, http.MethodGet, "/api/v1/media", f.adminTok)
	if status != http.StatusOK {
		t.Fatalf("status %d body %v", status, body)
	}
	media = body["media"].([]any)
	if len(media) != 2 {
		t.Fatalf("media length %d, want 2", len(media))
	}
	first := media[0].(map[string]any)
	if first["track_ref"] != "local:newer" || first["title"] != "Newer" || first["artist"] != "B" ||
		first["duration_ms"] != float64(30) || first["size_bytes"] != float64(40) ||
		first["uploaded_by"] != "two" || first["created_at"] != float64(200) {
		t.Fatalf("unexpected first media: %v", first)
	}
}

func TestDeleteMediaEndpoint(t *testing.T) {
	f := setupMediaEndpoints(t)
	ctx := context.Background()
	mediaPath := filepath.Join(f.mediaDir, "song.mp3")
	if err := os.WriteFile(mediaPath, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AddMediaFile(ctx, store.MediaFile{ID: "song", Filename: "song.mp3", Title: "Song", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(f.cacheDir, "song.cache")
	if err := os.WriteFile(cachePath, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutCacheRow(ctx, store.CacheRow{TrackRef: "local:song", FilePath: cachePath, SizeBytes: 6, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	status, body := mediaEndpointRequest(t, f, http.MethodDelete, "/api/v1/media/ncm:song", f.adminTok)
	if status != http.StatusBadRequest || errCode(t, body) != "bad_request" {
		t.Fatalf("non-local ref: status %d body %v", status, body)
	}
	status, body = mediaEndpointRequest(t, f, http.MethodDelete, "/api/v1/media/local:song", f.adminTok)
	if status != http.StatusOK || body["deleted"] != "local:song" {
		t.Fatalf("delete: status %d body %v", status, body)
	}
	if _, err := os.Stat(mediaPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("media file still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache file still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := f.st.GetCacheRow(ctx, "local:song"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cache row after delete: %v, want sql.ErrNoRows", err)
	}
	if _, err := f.local.GetTrack(ctx, provider.TrackRef("local:song")); !errors.Is(err, local.ErrNotFound) {
		t.Fatalf("GetTrack after delete: %v, want local.ErrNotFound", err)
	}

	status, body = mediaEndpointRequest(t, f, http.MethodDelete, "/api/v1/media/local:song", f.adminTok)
	if status != http.StatusNotFound || errCode(t, body) != "not_found" {
		t.Fatalf("second delete: status %d body %v", status, body)
	}
}

func TestDeleteMediaEndpointRole(t *testing.T) {
	f := setupMediaEndpoints(t)
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{name: "unauthenticated", want: http.StatusUnauthorized},
		{name: "wrong role", token: f.readerTok, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := mediaEndpointRequest(t, f, http.MethodDelete, fmt.Sprintf("/api/v1/media/local:%s", tc.name), tc.token)
			if status != tc.want {
				t.Fatalf("status %d body %v, want %d", status, body, tc.want)
			}
		})
	}
}
