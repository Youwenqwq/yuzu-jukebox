package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadIntegrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"integrations": [
			{"id": "generic-bridge", "token": "integration-secret"}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Integrations) != 1 || cfg.Integrations[0].ID != "generic-bridge" || cfg.Integrations[0].Token != "integration-secret" {
		t.Fatalf("integrations = %#v", cfg.Integrations)
	}
	if cfg.Addr != Default().Addr {
		t.Fatalf("default config fields were not retained: addr = %q", cfg.Addr)
	}
}
