package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestIntegrationRegistryValidation(t *testing.T) {
	tests := []struct {
		name    string
		entries []IntegrationCredential
	}{
		{name: "empty id", entries: []IntegrationCredential{{Token: "token-a"}}},
		{name: "blank id", entries: []IntegrationCredential{{ID: "  ", Token: "token-a"}}},
		{name: "empty token", entries: []IntegrationCredential{{ID: "bridge"}}},
		{name: "blank token", entries: []IntegrationCredential{{ID: "bridge", Token: "\t"}}},
		{name: "duplicate id", entries: []IntegrationCredential{
			{ID: "bridge", Token: "token-a"}, {ID: "bridge", Token: "token-b"},
		}},
		{name: "duplicate token", entries: []IntegrationCredential{
			{ID: "bridge-a", Token: "same-secret"}, {ID: "bridge-b", Token: "same-secret"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewIntegrationRegistry(test.entries)
			if !errors.Is(err, ErrInvalidIntegrationCredentials) {
				t.Fatalf("NewIntegrationRegistry error = %v, want ErrInvalidIntegrationCredentials", err)
			}
			if strings.Contains(err.Error(), "same-secret") {
				t.Fatalf("validation error leaked integration secret: %v", err)
			}
		})
	}
}

func TestIntegrationRegistryResolvesOnlyConfiguredTokens(t *testing.T) {
	registry, err := NewIntegrationRegistry([]IntegrationCredential{
		{ID: "bridge-a", Token: "token-a"},
		{ID: "bridge-b", Token: "token-b-with-a-different-length"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if integrationID, ok := registry.ResolveToken("token-b-with-a-different-length"); !ok || integrationID != "bridge-b" {
		t.Fatalf("ResolveToken = %q, %v", integrationID, ok)
	}
	if integrationID, ok := registry.ResolveToken("not-configured"); ok || integrationID != "" {
		t.Fatalf("unknown ResolveToken = %q, %v", integrationID, ok)
	}
	if !registry.Contains("bridge-a") || registry.Contains("missing") {
		t.Fatal("Contains did not reflect configured integration IDs")
	}
}
