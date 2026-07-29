// Package distribution implements the provider-neutral control plane for
// optional externally managed media accelerations. Locators remain opaque to
// Yuzu core.
package distribution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

var (
	ErrNoWork               = sql.ErrNoRows
	ErrInvalidLease         = store.ErrDistributionLeaseInvalid
	ErrExpiredLease         = store.ErrDistributionLeaseExpired
	ErrStaleProgress        = store.ErrDistributionProgressStale
	ErrInvalidCredential    = errors.New("invalid acceleration credential")
	ErrAccelerationDisabled = errors.New("acceleration is disabled")
)

type Candidate = store.DistributionCandidate
type Lease = store.DistributionLease
type Status = store.DistributionStatus
type Attempt = store.DistributionAttempt

type Service struct {
	st  *store.Store
	now func() time.Time
}

func New(st *store.Store) *Service {
	return &Service{st: st, now: time.Now}
}

func (s *Service) Store() *store.Store { return s.st }

func (s *Service) Request(ctx context.Context, accelerationID string, ref provider.TrackRef) error {
	if strings.TrimSpace(accelerationID) == "" || ref.String() == "" {
		return errors.New("acceleration id and track ref are required")
	}
	return s.st.RequestDistribution(ctx, accelerationID, ref.String(), s.now().UnixMilli())
}

func (s *Service) RequestCacheReady(ctx context.Context, ref provider.TrackRef) error {
	accelerations, err := s.st.ListCacheReadyAccelerations(ctx)
	if err != nil {
		return err
	}
	for _, acceleration := range accelerations {
		if err := s.Request(ctx, acceleration.ID, ref); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Candidate(ctx context.Context, accelerationID string, ref provider.TrackRef) (Candidate, bool, error) {
	candidate, err := s.st.GetDistributionCandidate(ctx, accelerationID, ref.String())
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, false, nil
	}
	return candidate, err == nil, err
}

func (s *Service) Claim(
	ctx context.Context,
	acceleration store.Acceleration,
	owner string,
	ttl time.Duration,
) (Lease, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Lease{}, errors.New("empty lease owner")
	}
	if !acceleration.Enabled {
		return Lease{}, ErrAccelerationDisabled
	}
	maximum := time.Duration(acceleration.LeaseTTLSeconds) * time.Second
	if maximum <= 0 {
		maximum = 10 * time.Minute
	}
	if ttl <= 0 || ttl > maximum {
		ttl = maximum
	}
	now := s.now()
	return s.st.ClaimDistribution(ctx, acceleration.ID, owner, randomID("dl_"),
		now.UnixMilli(), now.Add(ttl).UnixMilli())
}

func (s *Service) Lease(ctx context.Context, accelerationID, leaseID string) (Lease, error) {
	lease, err := s.st.GetDistributionLease(ctx, leaseID)
	if err != nil {
		return Lease{}, err
	}
	if lease.AccelerationID != accelerationID {
		return Lease{}, ErrInvalidLease
	}
	if lease.ExpiresAt <= s.now().UnixMilli() {
		return Lease{}, ErrExpiredLease
	}
	return lease, nil
}

func (s *Service) Progress(
	ctx context.Context,
	acceleration store.Acceleration,
	leaseID, owner, phase string,
	sourceBytes, uploadBytes, totalBytes int64,
) (Lease, error) {
	if sourceBytes < 0 || uploadBytes < 0 || totalBytes < 0 {
		return Lease{}, errors.New("distribution progress bytes must not be negative")
	}
	now := s.now()
	maximum := time.Duration(acceleration.LeaseTTLSeconds) * time.Second
	if maximum <= 0 {
		maximum = 10 * time.Minute
	}
	return s.st.UpdateDistributionProgress(ctx, leaseID, owner, phase,
		sourceBytes, uploadBytes, totalBytes, now.UnixMilli(), now.Add(maximum).UnixMilli())
}

func (s *Service) Complete(
	ctx context.Context,
	accelerationID, leaseID, owner string,
	candidate Candidate,
) error {
	candidate.AccelerationID = accelerationID
	if candidate.Layout == "" {
		candidate.Layout = "object"
	}
	if candidate.ContentVersion == "" || candidate.Locator == "" ||
		candidate.SizeBytes <= 0 || candidate.ContentType == "" {
		return errors.New("incomplete distribution candidate")
	}
	return s.st.CompleteDistribution(ctx, leaseID, owner, candidate, s.now().UnixMilli())
}

func (s *Service) Fail(
	ctx context.Context,
	accelerationID, leaseID, owner, message string,
	retryAfter time.Duration,
) error {
	lease, err := s.Lease(ctx, accelerationID, leaseID)
	if err != nil {
		return err
	}
	if lease.Owner != owner {
		return ErrInvalidLease
	}
	if retryAfter < 0 {
		retryAfter = 0
	}
	now := s.now()
	return s.st.FailDistribution(ctx, leaseID, owner, strings.TrimSpace(message),
		now.UnixMilli(), now.Add(retryAfter).UnixMilli())
}

func (s *Service) Heartbeat(
	ctx context.Context,
	accelerationID, owner, version, state, leaseID, trackRef string,
	capabilities []string,
	backendHealthy bool,
	lastError string,
) error {
	if owner = strings.TrimSpace(owner); owner == "" {
		return errors.New("publisher owner is required")
	}
	switch state {
	case "idle", "busy", "degraded":
	default:
		return fmt.Errorf("invalid publisher state %q", state)
	}
	encodedCapabilities, err := json.Marshal(capabilities)
	if err != nil {
		return err
	}
	return s.st.UpsertAccelerationPublisher(ctx, store.AccelerationPublisher{
		AccelerationID: accelerationID, Owner: owner, Version: strings.TrimSpace(version),
		State: state, LeaseID: leaseID, TrackRef: trackRef,
		Capabilities: string(encodedCapabilities), BackendHealthy: backendHealthy,
		LastError: strings.TrimSpace(lastError), LastSeenAt: s.now().UnixMilli(),
	})
}

func (s *Service) AddMetric(ctx context.Context, accelerationID, name string, delta int64) error {
	return s.st.AddDistributionMetric(ctx, accelerationID, name, delta, s.now().UnixMilli())
}

func (s *Service) Metrics(ctx context.Context, accelerationID string) (map[string]int64, Status, error) {
	metrics, err := s.st.DistributionMetrics(ctx, accelerationID)
	if err != nil {
		return nil, Status{}, err
	}
	status, err := s.st.DistributionStatus(ctx, accelerationID, s.now().UnixMilli())
	return metrics, status, err
}

func (s *Service) Metrics24Hours(ctx context.Context, accelerationID string) (map[string]int64, error) {
	return s.st.DistributionMetricsSince(ctx, accelerationID,
		s.now().Add(-24*time.Hour).UnixMilli())
}

func NewCredential(purpose string) (string, []byte, error) {
	prefix := map[string]string{
		"publisher": "yza_pub_",
		"edge":      "yza_edge_",
		"signer":    "yza_signer_",
	}[purpose]
	if prefix == "" {
		return "", nil, fmt.Errorf("unknown acceleration credential purpose %q", purpose)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	token := prefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, HashCredential(token), nil
}

func HashCredential(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func randomID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
