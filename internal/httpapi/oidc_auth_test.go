package httpapi

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// oidcTestIdP 内存版 Zitadel 替身：discovery（含 userinfo_endpoint）+ JWKS +
// 固定 userinfo 响应。sign 用同一把 RS256 私钥手工签发 id_token。
type oidcTestIdP struct {
	t       *testing.T
	server  *httptest.Server
	key     *rsa.PrivateKey
	userinfo map[string]any
}

func newOIDCTestIdP(t *testing.T, userinfo map[string]any) *oidcTestIdP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &oidcTestIdP{t: t, key: k, userinfo: userinfo}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":             f.server.URL,
			"jwks_uri":           f.server.URL + "/keys",
			"userinfo_endpoint":  f.server.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(k.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.PublicKey.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-token-1" {
			t.Errorf("userinfo bearer = %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(f.userinfo)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *oidcTestIdP) sign(payload map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"k1"}`))
	body, _ := json.Marshal(payload)
	body64 := base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(header + "." + body64))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return header + "." + body64 + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *oidcTestIdP) token(extra map[string]any) string {
	claims := map[string]any{
		"iss": f.server.URL, "aud": "yuzu-web", "sub": "user-42",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	return f.sign(claims)
}

func newOIDCTestServer(t *testing.T, idp *oidcTestIdP, roleMap map[string][]string) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "oidc.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	validator := auth.NewOIDCValidator(idp.server.URL, "yuzu-web")
	s := &Server{
		oidc: validator, oidcRoleMap: roleMap,
		authm: auth.NewManager("", st), st: st,
	}
	return s, st
}

func performOIDCLogin(t *testing.T, s *Server, idToken, accessToken string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"id_token": idToken, "access_token": accessToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/oidc", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.oidcAuth(w, r)
	return w
}

func decodeOIDCLogin(t *testing.T, rec *httptest.ResponseRecorder) (auth.Identity, string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("oidc login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Identity    auth.Identity `json:"identity"`
		SessionToken string       `json:"session_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode oidc login: %v", err)
	}
	if got.SessionToken == "" || got.Identity.ID == "" {
		t.Fatalf("missing session/identity: %#v", got)
	}
	return got.Identity, got.SessionToken
}

// TestOIDCAuthAvatarFromUserinfo：Zitadel 默认 id_token 不带 profile claims，
// avatar 只能经 userinfo 补齐（与 preferred_username 同路）。
func TestOIDCAuthAvatarFromUserinfo(t *testing.T) {
	idp := newOIDCTestIdP(t, map[string]any{
		"sub":                "user-42",
		"preferred_username": "youko",
		"picture":            "https://id.example/assets/v1/org1/avatar-key",
		"urn:zitadel:iam:org:project:123:roles": map[string]any{
			"jukebox-admin": map[string]any{},
		},
	})
	s, _ := newOIDCTestServer(t, idp, map[string][]string{
		"jukebox-admin": {auth.RoleRoomAdmin},
	})

	// id_token 无 preferred_username / picture（Zitadel 默认行为）
	identity, _ := decodeOIDCLogin(t, performOIDCLogin(t, s, idp.token(nil), "access-token-1"))

	if identity.Name != "youko" {
		t.Fatalf("name = %q, want youko (from userinfo)", identity.Name)
	}
	if identity.Avatar != "https://id.example/assets/v1/org1/avatar-key" {
		t.Fatalf("avatar = %q, want userinfo picture", identity.Avatar)
	}
	if !identity.HasRole(auth.RoleRoomAdmin) {
		t.Fatalf("mapped roles missing: %v", identity.Roles)
	}
}

// TestOIDCAuthAvatarFromIDToken：客户端开了 IDTokenUserinfoAssertion 时
// id_token 自带 picture，userinfo 兜底不得覆盖。
func TestOIDCAuthAvatarFromIDToken(t *testing.T) {
	idp := newOIDCTestIdP(t, map[string]any{
		"sub":                "user-42",
		"preferred_username": "youko",
		"picture":            "https://id.example/assets/v1/other-key",
	})
	s, _ := newOIDCTestServer(t, idp, nil)

	identity, _ := decodeOIDCLogin(t, performOIDCLogin(t, s,
		idp.token(map[string]any{
			"preferred_username": "youko",
			"picture":            "https://id.example/assets/v1/org1/avatar-key",
		}),
		"access-token-1"))

	if identity.Avatar != "https://id.example/assets/v1/org1/avatar-key" {
		t.Fatalf("avatar = %q, want id_token picture", identity.Avatar)
	}
}

// TestOIDCAuthUserinfoSubjectMismatch：userinfo sub 与 id_token 不符必须拒绝。
func TestOIDCAuthUserinfoSubjectMismatch(t *testing.T) {
	idp := newOIDCTestIdP(t, map[string]any{
		"sub": "someone-else", "preferred_username": "evil",
	})
	s, st := newOIDCTestServer(t, idp, nil)

	rec := performOIDCLogin(t, s, idp.token(nil), "access-token-1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	expectedTarget := auth.OIDCIdentity(auth.OIDCClaims{Sub: "user-42"}, nil).ID
	entries, err := st.QueryAudit(context.Background(), store.AuditFilter{
		Action: "auth.login_failed",
		Target: expectedTarget,
	}, 50, 0)
	if err != nil {
		t.Fatalf("query failed OIDC audit: %v", err)
	}
	if len(entries) != 1 || !strings.Contains(string(entries[0].Detail), "oidc_subject_mismatch") {
		t.Fatalf("failed OIDC audit entries = %#v", entries)
	}
}

func TestOIDCAuthInvalidTokenAuditedWithoutToken(t *testing.T) {
	idp := newOIDCTestIdP(t, nil)
	s, st := newOIDCTestServer(t, idp, nil)

	rec := performOIDCLogin(t, s, "not.a.valid-token", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	entries, err := st.QueryAudit(context.Background(), store.AuditFilter{
		Action: "auth.login_failed",
	}, 50, 0)
	if err != nil {
		t.Fatalf("query failed OIDC audit: %v", err)
	}
	if len(entries) != 1 || entries[0].Target != "" ||
		!strings.Contains(string(entries[0].Detail), "oidc_validation_failed") {
		t.Fatalf("invalid-token audit entries = %#v", entries)
	}
	if strings.Contains(string(entries[0].Detail), "not.a.valid-token") {
		t.Fatal("invalid OIDC token leaked into audit detail")
	}
}

// TestOIDCAuthPrincipalPersistsAvatar：登录后 principal 落库头像，
// room_admin 可在 /api/v1/principals 看到。
func TestOIDCAuthPrincipalPersistsAvatar(t *testing.T) {
	idp := newOIDCTestIdP(t, map[string]any{
		"sub":                "user-42",
		"preferred_username": "youko",
		"picture":            "https://id.example/assets/v1/org1/avatar-key",
	})
	s, st := newOIDCTestServer(t, idp, map[string][]string{
		"jukebox-admin": {auth.RoleRoomAdmin},
	})
	// 让 userinfo 带角色，登录后成为 room_admin
	idp.userinfo["urn:zitadel:iam:org:project:123:roles"] = map[string]any{
		"jukebox-admin": map[string]any{},
	}
	identity, token := decodeOIDCLogin(t, performOIDCLogin(t, s, idp.token(nil), "access-token-1"))

	if _, err := st.GetPrincipal(context.Background(), identity.ID); err != nil {
		t.Fatalf("principal not persisted: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/principals", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.listPrincipals(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("principals status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "https://id.example/assets/v1/org1/avatar-key") {
		t.Fatalf("principals response missing avatar: %s", w.Body.String())
	}
}
