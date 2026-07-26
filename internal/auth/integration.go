package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

var ErrInvalidIntegrationCredentials = errors.New("invalid integration credentials")

// IntegrationRegistry authenticates persistent Integration credentials.
// Token hashes are queried from the store so create, rotation and disable take
// effect without a server restart.
type IntegrationRegistry struct {
	st *store.Store
}

func NewIntegrationRegistry(st *store.Store) *IntegrationRegistry {
	return &IntegrationRegistry{st: st}
}

// NewIntegrationToken returns a high-entropy bearer token and its SHA-256 hash.
// Only the hash may be persisted; the plaintext is returned once to the admin.
func NewIntegrationToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	token := "yzi_" + base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))
	return token, digest[:], nil
}

func HashIntegrationToken(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

// ResolveToken authenticates an active Integration and records credential use.
func (r *IntegrationRegistry) ResolveToken(ctx context.Context, token string) (store.Integration, error) {
	integration, err := r.ValidateToken(ctx, token)
	if err != nil {
		return store.Integration{}, err
	}
	if err := r.st.TouchIntegration(ctx, integration.ID, time.Now().UnixMilli()); err != nil {
		return store.Integration{}, err
	}
	return integration, nil
}

// ValidateToken rechecks a credential without recording another use. Actor
// issuance uses it after persisting the session so concurrent disable, rotation,
// or deletion cannot leave a newly issued session alive.
func (r *IntegrationRegistry) ValidateToken(ctx context.Context, token string) (store.Integration, error) {
	if r == nil || r.st == nil || token == "" {
		return store.Integration{}, ErrInvalidIntegrationCredentials
	}
	integration, err := r.st.ResolveIntegrationToken(ctx, HashIntegrationToken(token))
	if err != nil {
		return store.Integration{}, ErrInvalidIntegrationCredentials
	}
	return integration, nil
}
