// Package coverurl 服务端签发的封面代理目标（防伪造 token）。
//
// 客户端拿到的封面 URL 一律是服务端代理路径（spec §6.2.1 不变量）。曲目封面
// 按 TrackRef 寻址；非曲目实体（艺人/专辑/歌单）没有 Ref，封面目标由本包签发
// 成不可伪造的 token：base64url(provider\nurl).base64url(HMAC-SHA256)，密钥
// 派生自 secret_key。客户端只能回放服务端签发过的目标，无法指定任意 URL
// （拒绝 url 透传式开放代理，SSRF 面封闭）。
package coverurl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Signer 签发与校验封面 token。零值不可用。
type Signer struct {
	key []byte
}

// New 以密钥材料构造签发器。key 为空时签发的 token 为空串（调用方保持原始 URL）。
func New(key []byte) *Signer { return &Signer{key: key} }

func (s *Signer) hmac(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("cover-ext\n"))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Mint 签发 (providerID, rawURL) 的封面 token。
// 无密钥或 rawURL 为空时返回空串（调用方按旧行为保持原始 URL）。
func (s *Signer) Mint(providerID, rawURL string) string {
	if len(s.key) == 0 || rawURL == "" {
		return ""
	}
	payload := providerID + "\n" + rawURL
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.hmac(payload)
}

// Open 校验并解出 token 内的 provider 与源站 URL。
func (s *Signer) Open(token string) (providerID, rawURL string, ok bool) {
	if len(s.key) == 0 {
		return "", "", false
	}
	body, sig, found := strings.Cut(token, ".")
	if !found {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", "", false
	}
	payload := string(raw)
	if !hmac.Equal([]byte(sig), []byte(s.hmac(payload))) {
		return "", "", false
	}
	pid, u, found := strings.Cut(payload, "\n")
	if !found || pid == "" || u == "" {
		return "", "", false
	}
	return pid, u, true
}
