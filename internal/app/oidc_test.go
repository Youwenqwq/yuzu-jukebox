package app_test

import (
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

	"github.com/coder/websocket"

	"github.com/youwenqwq/yuzu-jukebox/internal/app"
	"github.com/youwenqwq/yuzu-jukebox/internal/config"
)

// oidcEnv：完整服务端 + 内存假 IdP（discovery/JWKS/签名）。
type oidcEnv struct {
	t   *testing.T
	srv *httptest.Server
	idp *httptest.Server
	key *rsa.PrivateKey
}

func newOIDCEnv(t *testing.T) *oidcEnv {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e := &oidcEnv{t: t, key: key}

	idpMux := http.NewServeMux()
	idpMux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"jwks_uri": e.idp.URL + "/keys"})
	})
	idpMux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}})
	})
	e.idp = httptest.NewServer(idpMux)
	t.Cleanup(e.idp.Close)

	dir := t.TempDir()
	cfg := config.Config{
		Addr:          "127.0.0.1:0",
		DBPath:        filepath.Join(dir, "test.db"),
		MediaDir:      filepath.Join(dir, "media"),
		CacheDir:      filepath.Join(dir, "cache"),
		CacheMaxBytes: 1 << 30,
		AdminPassword: "admin123",
	}
	cfg.OIDC = config.OIDCConfig{
		Enabled:  true,
		Issuer:   e.idp.URL,
		ClientID: "yuzu-cli",
		RoleMapping: map[string][]string{
			"jukebox-admin": {"room_admin", "media_admin"},
		},
	}
	a, err := app.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { a.Store.Close() })
	e.srv = httptest.NewServer(a.Handler)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *oidcEnv) sign(payload map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"k1"}`))
	body, _ := json.Marshal(payload)
	body64 := base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(header + "." + body64))
	sig, err := rsa.SignPKCS1v15(rand.Reader, e.key, crypto.SHA256, digest[:])
	if err != nil {
		e.t.Fatal(err)
	}
	return header + "." + body64 + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (e *oidcEnv) oidcLogin(t *testing.T, roles map[string]any) (identity map[string]any, token string) {
	t.Helper()
	claims := map[string]any{
		"iss": e.idp.URL, "aud": "yuzu-cli", "sub": "zitadel-user-7",
		"preferred_username": "soraneko",
		"exp":                time.Now().Add(time.Hour).Unix(),
	}
	if roles != nil {
		claims["urn:zitadel:iam:org:project:123:roles"] = roles
	}
	body, _ := json.Marshal(map[string]any{"id_token": e.sign(claims)})
	resp, err := http.Post(e.srv.URL+"/api/v1/auth/oidc", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("oidc login status %d", resp.StatusCode)
	}
	var out struct {
		Identity     map[string]any `json:"identity"`
		SessionToken string         `json:"session_token"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Identity, out.SessionToken
}

func TestOIDCEndToEnd(t *testing.T) {
	e := newOIDCEnv(t)

	// 1. 带 jukebox-admin 角色登录 → 映射出管理角色
	id, token := e.oidcLogin(t, map[string]any{
		"jukebox-admin": map[string]string{"999": "org.example"},
	})
	if id["kind"] != "oidc" || id["name"] != "soraneko" {
		t.Fatalf("bad identity: %v", id)
	}
	roles, _ := id["roles"].([]any)
	var hasRoomAdmin bool
	for _, r := range roles {
		if r == "room_admin" {
			hasRoomAdmin = true
		}
	}
	if !hasRoomAdmin {
		t.Fatalf("role mapping failed: %v", roles)
	}

	// 2. session token 调管理端点（建房）→ 证明角色端到端生效
	body, _ := json.Marshal(map[string]any{"id": "oidc-room", "name": "OIDC 测试房"})
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/rooms", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create room with oidc token: status %d", resp.StatusCode)
	}

	// 3. WS 用 session_token 认证 → auth.ok
	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/v1"
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	authMsg, _ := json.Marshal(map[string]any{
		"type": "auth", "ref": "a1", "data": map[string]any{"session_token": token},
	})
	if err := conn.Write(context.Background(), websocket.MessageText, authMsg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, reply, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reply), "auth.ok") || !strings.Contains(string(reply), "soraneko") {
		t.Fatalf("unexpected ws auth reply: %s", reply)
	}

	// 4. 无角色用户 → 只有基础角色，建房应 403
	_, memberToken := e.oidcLogin(t, nil)
	req2, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/rooms", strings.NewReader(string(body)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+memberToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 403 {
		t.Fatalf("member should not create room: status %d", resp2.StatusCode)
	}

	// 5. 伪造 token → 401
	bad, _ := json.Marshal(map[string]any{"id_token": "not.a.token"})
	resp3, err := http.Post(e.srv.URL+"/api/v1/auth/oidc", "application/json", strings.NewReader(string(bad)))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 401 {
		t.Fatalf("bad token: status %d", resp3.StatusCode)
	}
}
