// Package distribution implements the provider-neutral control plane for
// optional media distribution backends. Locators remain opaque to Yuzu core.
package distribution

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

var (
	ErrNoWork       = sql.ErrNoRows
	ErrInvalidLease = store.ErrDistributionLeaseInvalid
	ErrExpiredLease = store.ErrDistributionLeaseExpired
)

type Candidate = store.DistributionCandidate
type Lease = store.DistributionLease
type Status = store.DistributionStatus

type Service struct {
	st       *store.Store
	backend  string
	leaseTTL time.Duration
	now      func() time.Time
}

func New(st *store.Store, backend string, leaseTTL time.Duration) *Service {
	backend = strings.TrimSpace(backend)
	if leaseTTL <= 0 {
		leaseTTL = 10 * time.Minute
	}
	return &Service{st: st, backend: backend, leaseTTL: leaseTTL, now: time.Now}
}

func (s *Service) Backend() string { return s.backend }

func (s *Service) Request(ctx context.Context, ref provider.TrackRef) error {
	if ref.String() == "" {
		return errors.New("empty track ref")
	}
	return s.st.RequestDistribution(ctx, s.backend, ref.String(), s.now().UnixMilli())
}

func (s *Service) Candidate(ctx context.Context, ref provider.TrackRef) (Candidate, bool, error) {
	candidate, err := s.st.GetDistributionCandidate(ctx, s.backend, ref.String())
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, false, nil
	}
	return candidate, err == nil, err
}

func (s *Service) Claim(ctx context.Context, owner string, ttl time.Duration) (Lease, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Lease{}, errors.New("empty lease owner")
	}
	if ttl <= 0 || ttl > s.leaseTTL {
		ttl = s.leaseTTL
	}
	now := s.now()
	return s.st.ClaimDistribution(
		ctx, s.backend, owner, randomID("dl_"),
		now.UnixMilli(), now.Add(ttl).UnixMilli(),
	)
}

func (s *Service) Lease(ctx context.Context, leaseID string) (Lease, error) {
	lease, err := s.st.GetDistributionLease(ctx, leaseID)
	if err != nil {
		return Lease{}, err
	}
	if lease.Backend != s.backend {
		return Lease{}, ErrInvalidLease
	}
	if lease.ExpiresAt <= s.now().UnixMilli() {
		return Lease{}, ErrExpiredLease
	}
	return lease, nil
}

func (s *Service) Complete(ctx context.Context, leaseID, owner string, candidate Candidate) error {
	candidate.Backend = s.backend
	if candidate.Layout == "" {
		candidate.Layout = "object"
	}
	if candidate.ContentVersion == "" || candidate.Locator == "" || candidate.SizeBytes <= 0 || candidate.ContentType == "" {
		return errors.New("incomplete distribution candidate")
	}
	return s.st.CompleteDistribution(ctx, leaseID, owner, candidate, s.now().UnixMilli())
}

func (s *Service) Fail(ctx context.Context, leaseID, owner, message string, retryAfter time.Duration) error {
	if retryAfter < 0 {
		retryAfter = 0
	}
	now := s.now()
	return s.st.FailDistribution(
		ctx, leaseID, owner, strings.TrimSpace(message),
		now.UnixMilli(), now.Add(retryAfter).UnixMilli(),
	)
}

func (s *Service) AddMetric(ctx context.Context, name string, delta int64) error {
	return s.st.AddDistributionMetric(ctx, s.backend, name, delta, s.now().UnixMilli())
}

func (s *Service) Metrics(ctx context.Context) (map[string]int64, Status, error) {
	metrics, err := s.st.DistributionMetrics(ctx, s.backend)
	if err != nil {
		return nil, Status{}, err
	}
	status, err := s.st.DistributionStatus(ctx, s.backend, s.now().UnixMilli())
	return metrics, status, err
}

func randomID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
