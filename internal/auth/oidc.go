package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OIDCValidator 验证 IdP（如 Zitadel）签发的 ID token。
//
// 只接受 RS256：discovery 与 JWKS 懒加载并缓存，未知 kid 触发一次刷新
// （应对密钥轮换）。access token 在 Zitadel 默认是 opaque，本验证器
// 只处理 ID token（恒为 JWT）。
type OIDCValidator struct {
	issuer   string
	clientID string
	hc       *http.Client

	mu      sync.Mutex
	jwksURI string
	keys    map[string]*rsa.PublicKey // kid → 公钥
}

func NewOIDCValidator(issuer, clientID string) *OIDCValidator {
	return &OIDCValidator{
		issuer:   strings.TrimSuffix(issuer, "/"),
		clientID: clientID,
		hc:       &http.Client{Timeout: 10 * time.Second},
		keys:     map[string]*rsa.PublicKey{},
	}
}

// OIDCClaims 从 ID token 提取的最小身份集。
type OIDCClaims struct {
	Sub      string
	Username string   // preferred_username；缺失时回退 sub
	Roles    []string // Zitadel project role keys（仅角色名）
}

var (
	ErrOIDCToken   = errors.New("invalid oidc token")
	ErrOIDCExpired = errors.New("oidc token expired")
)

// Validate 验签并校验 iss/aud/exp，返回身份声明。
func (v *OIDCValidator) Validate(ctx context.Context, idToken string) (OIDCClaims, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return OIDCClaims{}, fmt.Errorf("%w: not a JWT", ErrOIDCToken)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := b64json(parts[0], &header); err != nil {
		return OIDCClaims{}, fmt.Errorf("%w: bad header", ErrOIDCToken)
	}
	if header.Alg != "RS256" {
		return OIDCClaims{}, fmt.Errorf("%w: alg %q, want RS256", ErrOIDCToken, header.Alg)
	}

	key, err := v.keyFor(ctx, header.Kid)
	if err != nil {
		return OIDCClaims{}, err
	}
	signed := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("%w: bad signature encoding", ErrOIDCToken)
	}
	digest := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return OIDCClaims{}, fmt.Errorf("%w: signature mismatch", ErrOIDCToken)
	}

	var payload map[string]json.RawMessage
	if err := b64json(parts[1], &payload); err != nil {
		return OIDCClaims{}, fmt.Errorf("%w: bad payload", ErrOIDCToken)
	}
	get := func(name string) string {
		var s string
		if raw, ok := payload[name]; ok {
			json.Unmarshal(raw, &s)
		}
		return s
	}
	if iss := get("iss"); iss != v.issuer {
		return OIDCClaims{}, fmt.Errorf("%w: issuer %q", ErrOIDCToken, iss)
	}
	if !audContains(payload["aud"], v.clientID) {
		return OIDCClaims{}, fmt.Errorf("%w: audience mismatch", ErrOIDCToken)
	}
	var exp int64
	if raw, ok := payload["exp"]; ok {
		json.Unmarshal(raw, &exp)
	}
	// 60s 时钟偏差宽限
	if time.Now().Unix() > exp+60 {
		return OIDCClaims{}, ErrOIDCExpired
	}

	claims := OIDCClaims{Sub: get("sub")}
	if claims.Sub == "" {
		return OIDCClaims{}, fmt.Errorf("%w: missing sub", ErrOIDCToken)
	}
	claims.Username = get("preferred_username")
	if claims.Username == "" {
		claims.Username = claims.Sub
	}
	claims.Roles = zitadelRoles(payload)
	return claims, nil
}

// OIDCIdentity 由 OIDC 声明构造 yuzu 身份。roles 为完整角色集
// （基础角色 + 映射结果，由调用方组装）。
func OIDCIdentity(c OIDCClaims, roles []string) Identity {
	sum := sha256.Sum256([]byte("oidc:" + c.Sub))
	return Identity{
		ID:    "o_" + hex.EncodeToString(sum[:])[:12],
		Name:  c.Username,
		Kind:  "oidc",
		Roles: roles,
	}
}

// zitadelRoles 收集所有 urn:zitadel:iam:org:project*:roles claim 的
// 角色名（对象 key）。claim 值形如 {"role-a": {"org-id": "org-domain"}}。
func zitadelRoles(payload map[string]json.RawMessage) []string {
	var out []string
	for k, raw := range payload {
		if !strings.HasPrefix(k, "urn:zitadel:iam:org:project") || !strings.HasSuffix(k, ":roles") {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) == nil {
			for role := range m {
				out = append(out, role)
			}
		}
	}
	return out
}

// keyFor 取 kid 对应公钥；未知 kid 时强制刷新 JWKS 一次（密钥轮换）。
func (v *OIDCValidator) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	key, ok := v.keys[kid]
	v.mu.Unlock()
	if ok {
		return key, nil
	}
	if err := v.refreshKeys(ctx); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: unknown key id %q", ErrOIDCToken, kid)
}

func (v *OIDCValidator) refreshKeys(ctx context.Context) error {
	v.mu.Lock()
	jwksURI := v.jwksURI
	v.mu.Unlock()

	if jwksURI == "" {
		var discovery struct {
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, v.issuer+"/.well-known/openid-configuration", &discovery); err != nil {
			return fmt.Errorf("oidc discovery: %w", err)
		}
		if discovery.JWKSURI == "" {
			return errors.New("oidc discovery: no jwks_uri")
		}
		jwksURI = discovery.JWKSURI
	}

	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := v.getJSON(ctx, jwksURI, &jwks); err != nil {
		return fmt.Errorf("oidc jwks: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	v.mu.Lock()
	v.jwksURI = jwksURI
	v.keys = keys
	v.mu.Unlock()
	return nil
}

func (v *OIDCValidator) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := v.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// audContains aud 可能是字符串或字符串数组。
func audContains(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == want
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		for _, a := range arr {
			if a == want {
				return true
			}
		}
	}
	return false
}

func b64json(seg string, out any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
