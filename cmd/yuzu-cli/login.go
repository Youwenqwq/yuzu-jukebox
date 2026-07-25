package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

// ---------- 会话缓存（OIDC 登录所得） ----------

type cachedSession struct {
	Server  string `json:"server"`
	Token   string `json:"token"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	SavedAt int64  `json:"saved_at"`
}

func sessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "yuzu-cli", "session.json"), nil
}

func loadSession(server string) *cachedSession {
	p, err := sessionPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var s cachedSession
	if json.Unmarshal(data, &s) != nil || s.Token == "" || s.Server != server {
		return nil
	}
	return &s
}

func saveSession(s cachedSession) error {
	p, err := sessionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(p, data, 0o600)
}

func clearSession() {
	if p, err := sessionPath(); err == nil {
		os.Remove(p)
	}
}

// restToken REST 通道统一取 token：缓存的 OIDC 会话优先，否则 guest 现场认证。
func restToken(ctx context.Context) (string, error) {
	if s := loadSession(*server); s != nil {
		return s.Token, nil
	}
	return client.RESTAuth(ctx, *server, *name, *password)
}

// ---------- login / logout ----------

func cmdLogin(ctx context.Context) error {
	cfg, err := client.RESTOIDCConfig(ctx, *server)
	if err != nil {
		return fmt.Errorf("服务器未启用 OIDC 或不可达: %w", err)
	}
	// 环境变量可覆盖服务端发现（调试用）
	if v := os.Getenv("YUZU_OIDC_ISSUER"); v != "" {
		cfg.Issuer = v
	}
	if v := os.Getenv("YUZU_OIDC_CLIENT_ID"); v != "" {
		cfg.ClientID = v
	}

	// 登录全程自带超时：用户在浏览器里确认可能要几分钟，
	// 绝不能用 withCtx 的 15s（否则确认完换发 session 时 ctx 已死）。
	flowCtx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	idToken, accessToken, err := client.DeviceFlowLogin(flowCtx, cfg.Issuer, cfg.ClientID,
		func(uri, code string) {
			fmt.Printf("请在浏览器中完成登录确认：\n  %s\n用户码: %s\n等待确认中...\n", uri, code)
		})
	if err != nil {
		return fmt.Errorf("设备授权流失败: %w", err)
	}

	// 服务端首次验证可能要拉 discovery+JWKS，给足 30s。
	authCtx, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	identity, token, err := client.RESTOIDCAuth(authCtx, *server, idToken, accessToken)
	if err != nil {
		return fmt.Errorf("服务端 OIDC 认证失败: %w", err)
	}
	if err := saveSession(cachedSession{
		Server: *server, Token: token, Name: identity.Name,
		Kind: identity.Kind, SavedAt: time.Now().Unix(),
	}); err != nil {
		return fmt.Errorf("会话缓存写入失败: %w", err)
	}
	fmt.Printf("ok：已登录为 %s (%s, %s)\n", identity.Name, identity.Kind, identity.ID)
	return nil
}

func cmdLogout() error {
	if s := loadSession(*server); s != nil {
		// 服务端吊销（尽力而为：服务器不可达也照常清本地）
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = client.RESTLogout(ctx, *server, s.Token)
		cancel()
	}
	clearSession()
	fmt.Println("ok")
	return nil
}

func cmdWhoami(ctx context.Context) error {
	if s := loadSession(*server); s != nil {
		fmt.Printf("%s (%s)，登录于 %s\n", s.Name, s.Kind,
			time.Unix(s.SavedAt, 0).Format("2006-01-02 15:04"))
		return nil
	}
	fmt.Printf("guest：%s（未登录，-name/-password 现场认证）\n", *name)
	return nil
}
