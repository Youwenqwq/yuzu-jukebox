package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrInvalidIntegrationCredentials = errors.New("invalid integration credentials")

// IntegrationCredential is a trusted integration identity configured by the
// server operator. Its token authenticates only the integration itself.
type IntegrationCredential struct {
	ID    string
	Token string
}

type integrationCredential struct {
	id          string
	tokenDigest [sha256.Size]byte
}

// IntegrationRegistry is an immutable lookup table built during app assembly.
type IntegrationRegistry struct {
	byID        map[string]struct{}
	credentials []integrationCredential
	ids         []string
}

func NewIntegrationRegistry(entries []IntegrationCredential) (*IntegrationRegistry, error) {
	registry := &IntegrationRegistry{
		byID:        make(map[string]struct{}, len(entries)),
		credentials: make([]integrationCredential, 0, len(entries)),
		ids:         make([]string, 0, len(entries)),
	}
	seenTokens := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		if strings.TrimSpace(entry.ID) == "" {
			return nil, fmt.Errorf("%w: entry %d has empty id", ErrInvalidIntegrationCredentials, i)
		}
		if strings.TrimSpace(entry.Token) == "" {
			return nil, fmt.Errorf("%w: integration %q has empty token", ErrInvalidIntegrationCredentials, entry.ID)
		}
		if _, exists := registry.byID[entry.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate id %q", ErrInvalidIntegrationCredentials, entry.ID)
		}
		if _, exists := seenTokens[entry.Token]; exists {
			return nil, fmt.Errorf("%w: duplicate token", ErrInvalidIntegrationCredentials)
		}
		registry.byID[entry.ID] = struct{}{}
		seenTokens[entry.Token] = struct{}{}
		registry.ids = append(registry.ids, entry.ID)
		registry.credentials = append(registry.credentials, integrationCredential{
			id:          entry.ID,
			tokenDigest: sha256.Sum256([]byte(entry.Token)),
		})
	}
	sort.Strings(registry.ids)
	return registry, nil
}

// ResolveToken returns the configured integration ID. Secret comparison is
// fixed-width and scans the entire registry so credential order is not exposed.
func (r *IntegrationRegistry) ResolveToken(token string) (string, bool) {
	if r == nil {
		return "", false
	}
	candidate := sha256.Sum256([]byte(token))
	matched := 0
	matchedID := ""
	for _, credential := range r.credentials {
		equal := subtle.ConstantTimeCompare(candidate[:], credential.tokenDigest[:])
		if equal == 1 {
			matchedID = credential.id
		}
		matched |= equal
	}
	return matchedID, matched == 1
}

func (r *IntegrationRegistry) Contains(id string) bool {
	if r == nil {
		return false
	}
	_, ok := r.byID[id]
	return ok
}

// IDs returns the configured integration IDs in stable order.
func (r *IntegrationRegistry) IDs() []string {
	if r == nil {
		return []string{}
	}
	return append([]string(nil), r.ids...)
}
