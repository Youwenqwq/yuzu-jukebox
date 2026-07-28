package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func cmdIdentityBindCode(ctx context.Context) error {
	return issueIdentityBindCode(ctx, *server, os.Stdout)
}

func issueIdentityBindCode(ctx context.Context, serverURL string, out io.Writer) error {
	session := loadSession(serverURL)
	if session == nil || session.Kind != "oidc" {
		return fmt.Errorf("当前服务器没有缓存的 OIDC 会话；请先运行 yuzu-cli login")
	}

	issued, err := client.RESTIssueExternalBindingCode(ctx, serverURL, session.Token)
	if err != nil {
		return fmt.Errorf("签发 external subject 绑定码失败: %w", err)
	}
	_, err = fmt.Fprintf(out, "code: %s\nexpires_at: %d\n到期时间: %s\n",
		issued.Code,
		issued.ExpiresAt,
		time.UnixMilli(issued.ExpiresAt).Local().Format(time.RFC3339),
	)
	return err
}
