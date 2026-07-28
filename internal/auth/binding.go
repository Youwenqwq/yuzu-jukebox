package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/shortcode"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

const bindingCodeTTL = 10 * time.Minute

var (
	ErrBindingRequiresOIDC           = errors.New("binding code requires an OIDC login")
	ErrBindingCodeInvalid            = store.ErrBindingCodeInvalid
	ErrBindingConflict               = store.ErrBindingConflict
	ErrBindingPrincipalUnavailable   = store.ErrBindingPrincipalUnavailable
	ErrBindingIntegrationUnavailable = store.ErrBindingIntegrationUnavailable
)

type BindingService struct {
	st   *store.Store
	now  func() time.Time
	rand io.Reader
}

type BindingCode struct {
	Code      string
	ExpiresAt int64
}

type ExternalBindingTarget struct {
	IntegrationID string
	AdapterID     string
	ScopeType     string
	ScopeID       string
	SubjectID     string
}

type BindingRedemption struct {
	Identity Identity
	Replayed bool
}

func NewBindingService(st *store.Store) *BindingService {
	return &BindingService{st: st, now: time.Now, rand: rand.Reader}
}

func (s *BindingService) Issue(ctx context.Context, identity Identity) (BindingCode, error) {
	if s == nil || s.st == nil || identity.ID == "" || identity.IntegrationID != "" || identity.OIDCSubject == "" {
		return BindingCode{}, ErrBindingRequiresOIDC
	}

	raw := make([]byte, 8)
	if _, err := io.ReadFull(s.rand, raw); err != nil {
		return BindingCode{}, err
	}
	canonical, ok := shortcode.Encode(raw)
	if !ok {
		return BindingCode{}, errors.New("insufficient binding code entropy")
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	expiresAt := now.Add(bindingCodeTTL).UnixMilli()
	hash := sha256.Sum256([]byte(canonical))
	if err := s.st.CreateExternalBindingCode(
		ctx, identity.ID, hash[:], now.UnixMilli(), expiresAt,
	); err != nil {
		return BindingCode{}, err
	}
	display, _ := shortcode.Format(canonical)
	return BindingCode{Code: display, ExpiresAt: expiresAt}, nil
}

func (s *BindingService) Redeem(
	ctx context.Context,
	integrationToken, code string,
	target ExternalBindingTarget,
) (BindingRedemption, error) {
	if s == nil || s.st == nil || integrationToken == "" ||
		anyEmpty(target.IntegrationID, target.AdapterID, target.ScopeType, target.ScopeID, target.SubjectID) {
		return BindingRedemption{}, ErrBindingCodeInvalid
	}
	canonical, ok := shortcode.Normalize(code)
	if !ok {
		return BindingRedemption{}, ErrBindingCodeInvalid
	}
	codeHash := sha256.Sum256([]byte(canonical))
	redemption, err := s.st.RedeemExternalBindingCode(
		ctx,
		codeHash[:],
		HashIntegrationToken(integrationToken),
		store.ExternalBindingTarget{
			IntegrationID: target.IntegrationID,
			AdapterID:     target.AdapterID,
			ScopeType:     target.ScopeType,
			ScopeID:       target.ScopeID,
			SubjectID:     target.SubjectID,
		},
		s.now().UnixMilli(),
	)
	if err != nil {
		return BindingRedemption{}, err
	}
	return BindingRedemption{
		Identity: identityFromPrincipal(redemption.Principal),
		Replayed: redemption.Replayed,
	}, nil
}

func anyEmpty(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}
