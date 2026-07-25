package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OIDCPublicConfig 服务端公开的 OIDC 客户端配置（GET /api/v1/auth/oidc/config）。
type OIDCPublicConfig struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
}

// RESTOIDCConfig 从服务端发现 OIDC 配置（零配置登录的第一步）。未启用返回错误。
func RESTOIDCConfig(ctx context.Context, server string) (OIDCPublicConfig, error) {
	var out OIDCPublicConfig
	err := restCall(ctx, server, "GET", "/api/v1/auth/oidc/config", "", nil, &out)
	return out, err
}

// ---------- Device Authorization Grant (RFC 8628) ----------

// deviceFlowHTTP 可注入以便测试。
var deviceFlowHTTP = &http.Client{Timeout: 15 * time.Second}

var (
	ErrDeviceDenied   = errors.New("device authorization denied")
	ErrDeviceExpired  = errors.New("device code expired before user confirmed")
	errDevicePending  = errors.New("authorization_pending")
	errDeviceSlowDown = errors.New("slow_down")
)

// DeviceFlowLogin 执行设备授权流：取设备码 → display 展示验证链接与用户码
// （用户在有浏览器的设备上确认）→ 轮询 token 端点直到拿到 id_token。
//
// display 只调用一次；轮询间隔遵循服务端 interval，slow_down 时 +5s；
// 总窗口受 expires_in 约束（Zitadel 当前为 300s）。
func DeviceFlowLogin(ctx context.Context, issuer, clientID string, display func(verificationURI, userCode string)) (idToken string, err error) {
	issuer = strings.TrimSuffix(issuer, "/")

	form := url.Values{"client_id": {clientID}, "scope": {"openid"}}
	var dev struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := postForm(ctx, issuer+"/oauth/v2/device_authorization", form, &dev); err != nil {
		return "", fmt.Errorf("device authorization: %w", err)
	}
	if dev.Interval <= 0 {
		dev.Interval = 5
	}
	uri := dev.VerificationURIComplete
	if uri == "" {
		uri = dev.VerificationURI
	}
	display(uri, dev.UserCode)

	poll := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {dev.DeviceCode},
		"client_id":   {clientID},
	}
	deadline := time.Now().Add(time.Duration(dev.ExpiresIn)*time.Second + 30*time.Second)
	interval := time.Duration(dev.Interval) * time.Second
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return "", ErrDeviceExpired
		}
		var tok struct {
			IDToken string `json:"id_token"`
		}
		err := postForm(ctx, issuer+"/oauth/v2/token", poll, &tok)
		switch {
		case err == nil:
			if tok.IDToken == "" {
				return "", fmt.Errorf("token response missing id_token")
			}
			return tok.IDToken, nil
		case errors.Is(err, errDevicePending):
			// 用户还没确认，继续等
		case errors.Is(err, errDeviceSlowDown):
			interval += 5 * time.Second
		default:
			return "", err
		}
	}
}

// postForm 提交表单并解析 JSON 响应；OAuth 错误响应映射为哨兵错误。
func postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := deviceFlowHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var oerr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &oerr) == nil {
			switch oerr.Error {
			case "authorization_pending":
				return errDevicePending
			case "slow_down":
				return errDeviceSlowDown
			case "access_denied":
				return ErrDeviceDenied
			case "expired_token":
				return ErrDeviceExpired
			}
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}
