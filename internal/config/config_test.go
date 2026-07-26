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
