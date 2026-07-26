package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestNewIntegrationTokenIsOpaqueAndHashable(t *testing.T) {
	first, firstHash, err := NewIntegrationToken()
	if err != nil {
		t.Fatal(err)
	}
	second, secondHash, err := NewIntegrationToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "yzi_") || first == second {
		t.Fatalf("tokens are not distinct opaque credentials: %q %q", first, second)
	}
	if len(firstHash) != 32 || len(secondHash) != 32 {
		t.Fatalf("hash lengths = %d, %d", len(firstHash), len(secondHash))
	}
	if string(firstHash) != string(HashIntegrationToken(first)) {
		t.Fatal("returned hash does not match token")
	}
}

func TestIntegrationRegistryUsesCurrentPersistentCredential(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const token = "integration-secret"
	if _, err := st.CreateIntegration(ctx, "bridge", "Bridge", HashIntegrationToken(token)); err != nil {
		t.Fatal(err)
	}
	registry := NewIntegrationRegistry(st)
	resolved, err := registry.ResolveToken(ctx, token)
	if err != nil || resolved.ID != "bridge" {
		t.Fatalf("ResolveToken = %#v, %v", resolved, err)
	}
	if resolved, err := registry.ResolveToken(ctx, "wrong"); !errors.Is(err, ErrInvalidIntegrationCredentials) || resolved.ID != "" {
		t.Fatalf("wrong token ResolveToken = %#v, %v", resolved, err)
	}
	if _, err := st.UpdateIntegration(ctx, "bridge", "Bridge", false); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveToken(ctx, token); !errors.Is(err, ErrInvalidIntegrationCredentials) {
		t.Fatalf("disabled token error = %v", err)
	}
}
