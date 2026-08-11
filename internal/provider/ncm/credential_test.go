package ncm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// testProviderWithStore 构造带 store 的 Provider（凭据测试用）。
func testProviderWithStore(t *testing.T, server *httptest.Server) (*Provider, *store.Store) {
	t.Helper()
	t.Cleanup(server.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(server.URL, "", st), st
}

func TestCredentialStatusUnset(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer server.Close()
	p, _ := testProviderWithStore(t, server)

	if got := p.CredentialStatus(context.Background()); got != "unset" {
		t.Fatalf("CredentialStatus() = %q, want unset", got)
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests)
	}
}

func TestCredentialStatusValid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/status" {
			t.Errorf("path = %q, want /login/status", r.URL.Path)
		}
		if got := r.URL.Query().Get("cookie"); got != "MUSIC_U=valid" {
			t.Errorf("cookie = %q, want MUSIC_U=valid", got)
		}
		w.Header().Set("Content-Type", "application/json")
		// /login/status 契约：业务 code 嵌在 data 内，顶层无 code 字段。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"code":    200,
				"account": map[string]any{"id": 270169361},
				"profile": map[string]any{"userId": 270169361, "nickname": "邮文", "avatarUrl": "https://a/1"},
			},
		})
	}))
	p, st := testProviderWithStore(t, server)
	if err := st.UpsertCredential(context.Background(), "ncm", "MUSIC_U=valid", "ok"); err != nil {
		t.Fatal(err)
	}

	if got := p.CredentialStatus(context.Background()); got != "ok" {
		t.Fatalf("CredentialStatus() = %q, want ok", got)
	}
}

func TestCredentialStatusInvalid(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		// Enhanced 分支：code 200 但 account/profile 为 null（垃圾 cookie）。
		{name: "enhanced null profile", body: `{"data":{"code":200,"account":null,"profile":null}}`},
		// 原版分支：未登录直接返回 code 301，无 data 包装。
		{name: "upstream 301", body: `{"code":301,"message":"需要登录"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			p, st := testProviderWithStore(t, server)
			if err := st.UpsertCredential(context.Background(), "ncm", "MUSIC_U=garbage", "ok"); err != nil {
				t.Fatal(err)
			}

			if got := p.CredentialStatus(context.Background()); got != "invalid" {
				t.Fatalf("CredentialStatus() = %q, want invalid", got)
			}
		})
	}
}

func TestSetCredentialValid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"code":    200,
				"account": map[string]any{"id": 270169361},
				"profile": map[string]any{"userId": 270169361, "nickname": "邮文", "avatarUrl": "https://a/1"},
			},
		})
	}))
	p, st := testProviderWithStore(t, server)

	if err := p.SetCredential(context.Background(), "MUSIC_U=fresh"); err != nil {
		t.Fatalf("SetCredential() error = %v", err)
	}
	if status, err := st.GetCredentialStatus(context.Background(), "ncm"); err != nil || status != "ok" {
		t.Fatalf("credential status = %q err=%v, want ok", status, err)
	}
	owner, ok, err := st.GetCredentialOwner(context.Background(), "ncm")
	if err != nil || !ok {
		t.Fatalf("GetCredentialOwner: ok=%v err=%v", ok, err)
	}
	if owner.Account.UID != "270169361" || owner.Account.Name != "邮文" {
		t.Fatalf("account profile = %+v", owner.Account)
	}
}

func TestSetCredentialRejectsBadCookie(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"code":200,"account":null,"profile":null}}`))
	}))
	p, st := testProviderWithStore(t, server)

	if err := p.SetCredential(context.Background(), "MUSIC_U=garbage"); err == nil {
		t.Fatal("SetCredential() error = nil, want rejection")
	}
	if status, err := st.GetCredentialStatus(context.Background(), "ncm"); err != nil || status != "invalid" {
		t.Fatalf("credential status = %q err=%v, want invalid", status, err)
	}
}
