package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

const (
	bindingCodeTTL    = 10 * time.Minute
	bindingCodeLength = 12
)

var (
	ErrBindingRequiresOIDC           = errors.New("binding code requires an OIDC login")
	ErrBindingCodeInvalid            = store.ErrBindingCodeInvalid
	ErrBindingConflict               = store.ErrBindingConflict
	ErrBindingPrincipalUnavailable   = store.ErrBindingPrincipalUnavailable
	ErrBindingIntegrationUnavailable = store.ErrBindingIntegrationUnavailable
)

var bindingCodeEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

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
	canonical := bindingCodeEncoding.EncodeToString(raw)[:bindingCodeLength]
	now := s.now().UTC().Truncate(time.Millisecond)
	expiresAt := now.Add(bindingCodeTTL).UnixMilli()
	hash := sha256.Sum256([]byte(canonical))
	if err := s.st.CreateExternalBindingCode(
		ctx, identity.ID, hash[:], now.UnixMilli(), expiresAt,
	); err != nil {
		return BindingCode{}, err
	}
	return BindingCode{Code: formatBindingCode(canonical), ExpiresAt: expiresAt}, nil
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
	canonical, ok := normalizeBindingCode(code)
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

func normalizeBindingCode(code string) (string, bool) {
	canonical := strings.Map(func(r rune) rune {
		if r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, code)
	if len(canonical) != bindingCodeLength {
		return "", false
	}
	for _, r := range canonical {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r) {
			return "", false
		}
	}
	return canonical, true
}

func formatBindingCode(code string) string {
	return code[:4] + "-" + code[4:8] + "-" + code[8:]
}

func anyEmpty(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}
