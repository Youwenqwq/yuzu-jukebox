package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialEncryptionRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.UpsertCredential(ctx, "ncm", "MUSIC_U=secret-value", "ok"); err != nil {
		t.Fatal(err)
	}

	// 落盘必须是 enc1: 前缀的密文
	var raw string
	if err := st.db.QueryRow(`SELECT payload FROM credentials ORDER BY id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "enc1:") {
		t.Fatalf("payload not encrypted: %q", raw)
	}
	if strings.Contains(raw, "secret-value") {
		t.Fatal("plaintext leaked into stored payload")
	}

	// 读取路径透明解密
	got, err := st.GetCredential(ctx, "ncm")
	if err != nil {
		t.Fatal(err)
	}
	if got != "MUSIC_U=secret-value" {
		t.Fatalf("decrypt round trip: %q", got)
	}
}

func TestCredentialLegacyPlaintextPassthrough(t *testing.T) {
	key := make([]byte, 32)
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// 直插一条无前缀的历史明文行
	if _, err := st.db.Exec(`INSERT INTO credentials (provider, payload, status, last_check_at) VALUES ('bili','SESSDATA=legacy','ok',0)`); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCredential(ctx, "bili")
	if err != nil {
		t.Fatal(err)
	}
	if got != "SESSDATA=legacy" {
		t.Fatalf("legacy plaintext should pass through, got %q", got)
	}
}

func TestCredentialEncryptedButNoKey(t *testing.T) {
	key := make([]byte, 32)
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCredential(context.Background(), "ncm", "MUSIC_U=x", "ok"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// 无 key 重开：加密行必须报可读错误而非静默返回密文
	st2, err := Open(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := st2.GetCredential(context.Background(), "ncm"); err == nil {
		t.Fatal("expected error reading encrypted credential without key")
	}
}
