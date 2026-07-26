package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func cacheEndpointRequest(t *testing.T, f mediaEndpointFixture, method, path, token, requestBody string) (int, map[string]any) {
	t.Helper()
	var body *bytes.Reader
	if requestBody == "" {
		body = bytes.NewReader(nil)
	} else {
		body = bytes.NewReader([]byte(requestBody))
	}
	req, err := http.NewRequest(method, f.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if requestBody != "" {
		req.Header.Set("Content-Type", "application/json")
	}
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

func putCacheEndpointRow(t *testing.T, f mediaEndpointFixture, ref string, size int64) string {
	t.Helper()
	path := filepath.Join(f.cacheDir, ref+".bin")
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := f.st.PutCacheRow(context.Background(), store.CacheRow{
		TrackRef: ref, FilePath: path, SizeBytes: size,
		LastAccessedAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestListCacheEndpointIncludesTotalAndLimit(t *testing.T) {
	f := setupMediaEndpoints(t)
	putCacheEndpointRow(t, f, "one", 5)
	putCacheEndpointRow(t, f, "two", 7)

	status, body := cacheEndpointRequest(t, f, http.MethodGet, "/api/v1/media/cache", f.adminTok, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if got := body["total_bytes"]; got != float64(12) {
		t.Fatalf("total_bytes = %v, want 12", got)
	}
	if got := body["max_bytes"]; got != float64(1<<20) {
		t.Fatalf("max_bytes = %v, want %d", got, 1<<20)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("entries = %#v, want two rows", body["entries"])
	}
}

func TestPruneCacheEndpointRoleAndValidation(t *testing.T) {
	f := setupMediaEndpoints(t)
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{name: "unauthenticated", want: http.StatusUnauthorized},
		{name: "wrong role", token: f.readerTok, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := cacheEndpointRequest(t, f, http.MethodPost, "/api/v1/media/cache/prune", tc.token, `{"unused_days":0}`)
			if status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name, body string
	}{
		{name: "missing", body: `{}`},
		{name: "negative", body: `{"unused_days":-1}`},
		{name: "fraction", body: `{"unused_days":1.5}`},
		{name: "string", body: `{"unused_days":"1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := cacheEndpointRequest(t, f, http.MethodPost, "/api/v1/media/cache/prune", f.adminTok, tc.body)
			if status != http.StatusBadRequest || errCode(t, body) != "bad_request" {
				t.Fatalf("status = %d, body = %v", status, body)
			}
		})
	}
}

func TestPruneCacheEndpointZeroClearsCache(t *testing.T) {
	f := setupMediaEndpoints(t)
	first := putCacheEndpointRow(t, f, "one", 5)
	second := putCacheEndpointRow(t, f, "two", 7)

	status, body := cacheEndpointRequest(t, f, http.MethodPost, "/api/v1/media/cache/prune", f.adminTok, `{"unused_days":0}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if body["evicted"] != float64(2) || body["freed_bytes"] != float64(12) {
		t.Fatalf("body = %v, want evicted=2 freed_bytes=12", body)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cache file %q still exists: %v", path, err)
		}
	}
}
