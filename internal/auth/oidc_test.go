package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeIdP 内存版 IdP：discovery + JWKS + 手工签发的 RS256 token。
type fakeIdP struct {
	t *testing.T
	s *httptest.Server
	k *rsa.PrivateKey
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, k: k}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"jwks_uri": f.s.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(k.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.PublicKey.E)).Bytes()),
		}}})
	})
	f.s = httptest.NewServer(mux)
	t.Cleanup(f.s.Close)
	return f
}

func (f *fakeIdP) sign(payload map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"k1"}`))
	body, _ := json.Marshal(payload)
	body64 := base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(header + "." + body64))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.k, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return header + "." + body64 + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *fakeIdP) baseClaims() map[string]any {
	return map[string]any{
		"iss": f.s.URL, "aud": "yuzu-cli", "sub": "user-42",
		"preferred_username": "youko",
		"picture":            "https://id.example/assets/v1/org1/avatar-key",
		"exp":                time.Now().Add(time.Hour).Unix(),
	}
}

func TestOIDCValidateHappyPath(t *testing.T) {
	f := newFakeIdP(t)
	v := NewOIDCValidator(f.s.URL, "yuzu-cli")

	claims := f.baseClaims()
	claims["urn:zitadel:iam:org:project:123:roles"] = map[string]any{
		"jukebox-admin": map[string]string{"999": "org.example"},
	}
	token := f.sign(claims)

	got, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sub != "user-42" || got.Username != "youko" {
		t.Fatalf("unexpected claims: %+v", got)
	}
	if got.Avatar != "https://id.example/assets/v1/org1/avatar-key" {
		t.Fatalf("picture not extracted: %q", got.Avatar)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "jukebox-admin" {
		t.Fatalf("roles not extracted: %v", got.Roles)
	}
}

func TestOIDCValidateMissingPicture(t *testing.T) {
	f := newFakeIdP(t)
	v := NewOIDCValidator(f.s.URL, "yuzu-cli")
	c := f.baseClaims()
	delete(c, "picture")
	got, err := v.Validate(context.Background(), f.sign(c))
	if err != nil {
		t.Fatal(err)
	}
	if got.Avatar != "" {
		t.Fatalf("want empty avatar, got %q", got.Avatar)
	}
}

func TestOIDCApplyUserinfo(t *testing.T) {
	f := newFakeIdP(t)
	v := NewOIDCValidator(f.s.URL, "yuzu-cli")
	// id_token 只带 sub（Zitadel 默认剥掉 profile claims）
	claims, err := v.Validate(context.Background(), f.sign(map[string]any{
		"iss": f.s.URL, "aud": "yuzu-cli", "sub": "user-42",
		"exp": time.Now().Add(time.Hour).Unix(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if claims.Username != "user-42" || claims.Avatar != "" {
		t.Fatalf("want bare sub identity, got %+v", claims)
	}

	claims.ApplyUserinfo(map[string]any{
		"sub":                "user-42",
		"preferred_username": "youko",
		"picture":            "https://id.example/assets/v1/org1/avatar-key",
		"urn:zitadel:iam:org:project:123:roles": map[string]any{
			"jukebox-admin": map[string]any{},
		},
	})
	if claims.Username != "youko" || claims.Avatar != "https://id.example/assets/v1/org1/avatar-key" {
		t.Fatalf("userinfo not applied: %+v", claims)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "jukebox-admin" {
		t.Fatalf("roles not merged: %v", claims.Roles)
	}
}

func TestOIDCApplyUserinfoKeepsIdTokenAvatar(t *testing.T) {
	// id_token 已带头像时（客户端开了 IDTokenUserinfoAssertion），userinfo 不该覆盖。
	claims := OIDCClaims{Sub: "s", Username: "u", Avatar: "https://id.example/a1"}
	claims.ApplyUserinfo(map[string]any{"picture": "https://id.example/a2"})
	if claims.Avatar != "https://id.example/a1" {
		t.Fatalf("id_token avatar overwritten: %q", claims.Avatar)
	}
}

func TestOIDCValidateRejects(t *testing.T) {
	f := newFakeIdP(t)
	v := NewOIDCValidator(f.s.URL, "yuzu-cli")
	ctx := context.Background()

	cases := map[string]func(map[string]any) string{
		"wrong issuer": func(c map[string]any) string {
			c["iss"] = "https://evil.example"
			return f.sign(c)
		},
		"wrong audience": func(c map[string]any) string {
			c["aud"] = "other-client"
			return f.sign(c)
		},
		"expired": func(c map[string]any) string {
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			return f.sign(c)
		},
		"tampered payload": func(c map[string]any) string {
			token := f.sign(c)
			parts := splitDot(token)
			// 改 payload 但不动签名
			c["sub"] = "admin"
			body, _ := json.Marshal(c)
			return parts[0] + "." + base64.RawURLEncoding.EncodeToString(body) + "." + parts[2]
		},
		"not a jwt": func(map[string]any) string { return "aaa.bbb" },
	}
	for name, makeToken := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Validate(ctx, makeToken(f.baseClaims())); err == nil {
				t.Fatal("expected rejection, got nil error")
			}
		})
	}
}

func TestOIDCValidateExpiredErrorKind(t *testing.T) {
	f := newFakeIdP(t)
	v := NewOIDCValidator(f.s.URL, "yuzu-cli")
	c := f.baseClaims()
	c["exp"] = time.Now().Add(-time.Hour).Unix()
	_, err := v.Validate(context.Background(), f.sign(c))
	if !errors.Is(err, ErrOIDCExpired) {
		t.Fatalf("want ErrOIDCExpired, got %v", err)
	}
}

func TestOIDCAudienceArray(t *testing.T) {
	f := newFakeIdP(t)
	v := NewOIDCValidator(f.s.URL, "yuzu-cli")
	c := f.baseClaims()
	c["aud"] = []string{"someone-else", "yuzu-cli"}
	if _, err := v.Validate(context.Background(), f.sign(c)); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCExtraClientIDs(t *testing.T) {
	f := newFakeIdP(t)
	v := NewOIDCValidator(f.s.URL, "yuzu-cli", "yuzu-web")
	ctx := context.Background()

	t.Run("extra client id accepted", func(t *testing.T) {
		c := f.baseClaims()
		c["aud"] = "yuzu-web"
		if _, err := v.Validate(ctx, f.sign(c)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("primary client id still accepted", func(t *testing.T) {
		if _, err := v.Validate(ctx, f.sign(f.baseClaims())); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown audience rejected", func(t *testing.T) {
		c := f.baseClaims()
		c["aud"] = "unknown-client"
		if _, err := v.Validate(ctx, f.sign(c)); err == nil {
			t.Fatal("expected rejection, got nil error")
		}
	})

	t.Run("audience array mixed hit", func(t *testing.T) {
		c := f.baseClaims()
		c["aud"] = []string{"someone-else", "yuzu-web"}
		if _, err := v.Validate(ctx, f.sign(c)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("audience array no hit rejected", func(t *testing.T) {
		c := f.baseClaims()
		c["aud"] = []string{"someone-else", "another-one"}
		if _, err := v.Validate(ctx, f.sign(c)); err == nil {
			t.Fatal("expected rejection, got nil error")
		}
	})
}

func TestOIDCClientIDsOrder(t *testing.T) {
	v := NewOIDCValidator("https://id.example", "yuzu-cli", "yuzu-web", "yuzu-admin")
	got := v.ClientIDs()
	want := []string{"yuzu-cli", "yuzu-web", "yuzu-admin"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
	if noExtra := NewOIDCValidator("https://id.example", "yuzu-cli"); len(noExtra.ClientIDs()) != 1 {
		t.Fatalf("no extras: want 1 client id, got %v", noExtra.ClientIDs())
	}
}

func TestOIDCUnknownKid(t *testing.T) {
	f := newFakeIdP(t)
	v := NewOIDCValidator(f.s.URL, "yuzu-cli")
	token := f.sign(f.baseClaims())
	// 换掉 header 里的 kid
	parts := splitDot(token)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"nope"}`))
	digest := sha256.Sum256([]byte(header + "." + parts[1]))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.k, crypto.SHA256, digest[:])
	token = header + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(sig)

	_, err := v.Validate(context.Background(), token)
	if err == nil || !errors.Is(err, ErrOIDCToken) {
		t.Fatalf("want unknown-kid error, got %v", err)
	}
}

func TestOIDCIdentityStable(t *testing.T) {
	a := OIDCIdentity(OIDCClaims{Sub: "s1", Username: "u"}, []string{RoleListener})
	b := OIDCIdentity(OIDCClaims{Sub: "s1", Username: "u"}, []string{RoleListener})
	c := OIDCIdentity(OIDCClaims{Sub: "s2", Username: "u"}, []string{RoleListener})
	if a.ID != b.ID || a.ID == c.ID || a.Kind != "oidc" || a.Name != "u" {
		t.Fatalf("identity derivation broken: %s %s %s", a.ID, b.ID, c.ID)
	}
	if a.Avatar != "" {
		t.Fatalf("want empty avatar when claim unset, got %q", a.Avatar)
	}
	withAvatar := OIDCIdentity(OIDCClaims{Sub: "s1", Username: "u", Avatar: "https://id.example/a"}, []string{RoleListener})
	if withAvatar.Avatar != "https://id.example/a" {
		t.Fatalf("avatar not carried into identity: %q", withAvatar.Avatar)
	}
}

func splitDot(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
