package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsRemovedIntegrationSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"integrations": [
			{"id": "generic-bridge", "token": "integration-secret"}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field \"integrations\"") {
		t.Fatalf("Load error = %v, want removed integrations field rejection", err)
	}
	if strings.Contains(err.Error(), "integration-secret") {
		t.Fatalf("Load error leaked removed token: %v", err)
	}
}

func TestDefaultUploadAndCacheObjectLimits(t *testing.T) {
	cfg := Default()
	if cfg.Media.MaxUploadBytes != 1<<30 {
		t.Fatalf("media.max_upload_bytes = %d, want %d", cfg.Media.MaxUploadBytes, int64(1<<30))
	}
	if cfg.Cache.MaxObjectBytes != 512<<20 {
		t.Fatalf("cache.max_object_bytes = %d, want %d", cfg.Cache.MaxObjectBytes, int64(512<<20))
	}
}

func TestDefaultSessionTTL(t *testing.T) {
	cfg := Default()
	if cfg.SessionTTLHours != 30*24 {
		t.Fatalf("session_ttl_hours = %d, want %d", cfg.SessionTTLHours, 30*24)
	}
	if cfg.SessionTTL() != 30*24*time.Hour {
		t.Fatalf("SessionTTL() = %v, want 30 days", cfg.SessionTTL())
	}

	cfg.SessionTTLHours = 12
	if cfg.SessionTTL() != 12*time.Hour {
		t.Fatalf("configured SessionTTL() = %v, want 12h", cfg.SessionTTL())
	}
}

func TestLoadWebSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"web": {"title": "客厅点歌台", "scheme": "light", "accent": "#6A8FD8", "oidc_client_id": "web-client"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Web.Title != "客厅点歌台" || cfg.Web.Scheme != "light" ||
		cfg.Web.Accent != "#6A8FD8" || cfg.Web.OIDCClientID != "web-client" {
		t.Fatalf("web = %+v", cfg.Web)
	}

	// 未配置 web 段时保持空值，前端回退到内置默认品牌。
	if got := Default().Web; got != (WebConfig{}) {
		t.Fatalf("default web = %+v, want zero", got)
	}
}

func TestLoadRejectsInvalidWebScheme(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"web": {"scheme": "auto"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "web.scheme") {
		t.Fatalf("Load error = %v, want web.scheme rejection", err)
	}
}
