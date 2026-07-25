// provider.go — Provider 与凭据管理命令实现。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func cmdProviders(ctx context.Context) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	providers, err := client.RESTListProviders(ctx, *server, token)
	if err != nil {
		return err
	}
	for _, p := range providers {
		status := p.CredentialStatus
		if status == "" {
			status = "-"
		}
		fmt.Printf("%-12s %s\n", p.ID, status)
	}
	return nil
}

func cmdCredential(ctx context.Context, providerID, payload string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	return client.RESTSetCredential(ctx, *server, token, providerID, payload)
}

// cmdQRLogin 二维码登录：渲染二维码并轮询，凭据由服务端在扫码成功后自动生效。
// 轮询不套全局 15s 超时（扫码是分钟级操作），单独给 5 分钟。
func cmdQRLogin(providerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	sess, err := client.RESTQRLoginStart(ctx, *server, token, providerID)
	if err != nil {
		return err
	}
	fmt.Println("请用网易云音乐 App 扫描以下二维码：")
	qrterminal.GenerateHalfBlock(sess.QRContent, qrterminal.L, os.Stdout)

	lastStatus := ""
	for {
		res, err := client.RESTQRLoginPoll(ctx, *server, token, providerID, sess.Key)
		if err != nil {
			return err
		}
		if res.Status != lastStatus {
			switch res.Status {
			case "waiting":
				fmt.Println("等待扫码…")
			case "scanned":
				fmt.Println("已扫码，请在 App 上确认登录…")
			}
			lastStatus = res.Status
		}
		switch res.Status {
		case "ok":
			fmt.Println("✓", res.Message)
			return nil
		case "expired":
			return fmt.Errorf("二维码已过期，请重新运行")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待超时")
		case <-time.After(2 * time.Second):
		}
	}
}
