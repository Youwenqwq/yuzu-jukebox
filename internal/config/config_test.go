package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
