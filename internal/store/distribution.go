package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrDistributionLeaseInvalid = errors.New("distribution lease invalid")
	ErrDistributionLeaseExpired = errors.New("distribution lease expired")
)

type DistributionLease struct {
	ID        string `json:"id"`
	Backend   string `json:"backend"`
	TrackRef  string `json:"track_ref"`
	Owner     string `json:"owner"`
	ExpiresAt int64  `json:"expires_at"`
	CreatedAt int64  `json:"created_at"`
}

type DistributionCandidate struct {
	Backend        string `json:"backend"`
	TrackRef       string `json:"track_ref"`
	ContentVersion string `json:"content_version"`
	Locator        string `json:"locator"`
	Layout         string `json:"layout"`
	SizeBytes      int64  `json:"size_bytes"`
	ContentType    string `json:"content_type"`
	ETag           string `json:"etag,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

type DistributionStatus struct {
	Requested int64 `json:"requested"`
	Pending   int64 `json:"pending"`
	Leased    int64 `json:"leased"`
	Ready     int64 `json:"ready"`
}

// RequestDistribution records demand for a track. It is deliberately
// idempotent: repeated player retries must not create duplicate upload work.
func (s *Store) RequestDistribution(ctx context.Context, backend, trackRef string, now int64) error {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO distribution_requests
		 (backend, track_ref, requested_at, updated_at, next_attempt_at)
		 VALUES (?, ?, ?, ?, 0)
		 ON CONFLICT(backend, track_ref) DO NOTHING`,
		backend, trackRef, now, now)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		_, err = s.db.ExecContext(ctx,
			`UPDATE distribution_requests SET updated_at = ?
			 WHERE backend = ? AND track_ref = ?`, now, backend, trackRef)
		return err
	}
	return s.AddDistributionMetric(ctx, backend, "requests", 1, now)
}

func (s *Store) GetDistributionCandidate(ctx context.Context, backend, trackRef string) (DistributionCandidate, error) {
	var candidate DistributionCandidate
	err := s.db.QueryRowContext(ctx,
		`SELECT backend, track_ref, content_version, locator, layout,
		        size_bytes, content_type, etag, created_at, updated_at
		 FROM distribution_candidates
		 WHERE backend = ? AND track_ref = ?`, backend, trackRef).
		Scan(
			&candidate.Backend, &candidate.TrackRef, &candidate.ContentVersion,
			&candidate.Locator, &candidate.Layout, &candidate.SizeBytes,
			&candidate.ContentType, &candidate.ETag,
			&candidate.CreatedAt, &candidate.UpdatedAt,
		)
	return candidate, err
}

// ClaimDistribution leases the oldest requested track that has no ready
// candidate and no live publisher. Expired leases are reclaimed atomically.
func (s *Store) ClaimDistribution(
	ctx context.Context,
	backend, owner, leaseID string,
	now, expiresAt int64,
) (DistributionLease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DistributionLease{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM distribution_leases WHERE expires_at <= ?`, now); err != nil {
		return DistributionLease{}, err
	}

	var trackRef string
	err = tx.QueryRowContext(ctx,
		`SELECT request.track_ref
		 FROM distribution_requests AS request
		 LEFT JOIN distribution_candidates AS candidate
		   ON candidate.backend = request.backend
		  AND candidate.track_ref = request.track_ref
		 LEFT JOIN distribution_leases AS lease
		   ON lease.backend = request.backend
		  AND lease.track_ref = request.track_ref
		 WHERE request.backend = ?
		   AND request.next_attempt_at <= ?
		   AND candidate.track_ref IS NULL
		   AND lease.id IS NULL
		 ORDER BY request.requested_at, request.track_ref
		 LIMIT 1`, backend, now).Scan(&trackRef)
	if err != nil {
		return DistributionLease{}, err
	}

	lease := DistributionLease{
		ID: leaseID, Backend: backend, TrackRef: trackRef, Owner: owner,
		ExpiresAt: expiresAt, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO distribution_leases
		 (id, backend, track_ref, owner, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		lease.ID, lease.Backend, lease.TrackRef, lease.Owner,
		lease.ExpiresAt, lease.CreatedAt); err != nil {
		return DistributionLease{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE distribution_requests
		 SET attempts = attempts + 1, updated_at = ?
		 WHERE backend = ? AND track_ref = ?`,
		now, backend, trackRef); err != nil {
		return DistributionLease{}, err
	}
	if err := addDistributionMetricTx(ctx, tx, backend, "leases", 1, now); err != nil {
		return DistributionLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return DistributionLease{}, err
	}
	return lease, nil
}

func (s *Store) GetDistributionLease(ctx context.Context, leaseID string) (DistributionLease, error) {
	var lease DistributionLease
	err := s.db.QueryRowContext(ctx,
		`SELECT id, backend, track_ref, owner, expires_at, created_at
		 FROM distribution_leases WHERE id = ?`, leaseID).
		Scan(&lease.ID, &lease.Backend, &lease.TrackRef, &lease.Owner, &lease.ExpiresAt, &lease.CreatedAt)
	return lease, err
}

func (s *Store) CompleteDistribution(
	ctx context.Context,
	leaseID, owner string,
	candidate DistributionCandidate,
	now int64,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	lease, requestedAt, err := distributionLeaseForUpdate(ctx, tx, leaseID, owner, now)
	if err != nil {
		return err
	}
	if candidate.Backend != lease.Backend || candidate.TrackRef != lease.TrackRef {
		return fmt.Errorf("%w: candidate does not match lease", ErrDistributionLeaseInvalid)
	}
	if candidate.Layout == "" {
		candidate.Layout = "object"
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO distribution_candidates
		 (backend, track_ref, content_version, locator, layout, size_bytes,
		  content_type, etag, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(backend, track_ref) DO UPDATE SET
		 content_version = excluded.content_version,
		 locator = excluded.locator,
		 layout = excluded.layout,
		 size_bytes = excluded.size_bytes,
		 content_type = excluded.content_type,
		 etag = excluded.etag,
		 updated_at = excluded.updated_at`,
		candidate.Backend, candidate.TrackRef, candidate.ContentVersion,
		candidate.Locator, candidate.Layout, candidate.SizeBytes,
		candidate.ContentType, candidate.ETag, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM distribution_leases WHERE id = ?`, leaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE distribution_requests
		 SET last_error = '', next_attempt_at = 0, updated_at = ?
		 WHERE backend = ? AND track_ref = ?`,
		now, lease.Backend, lease.TrackRef); err != nil {
		return err
	}
	metrics := map[string]int64{
		"publish_success":        1,
		"uploaded_bytes":         candidate.SizeBytes,
		"ready_latency_ms_total": max(now-requestedAt, 0),
		"ready_latency_samples":  1,
	}
	for name, delta := range metrics {
		if err := addDistributionMetricTx(ctx, tx, lease.Backend, name, delta, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) FailDistribution(
	ctx context.Context,
	leaseID, owner, message string,
	now, nextAttemptAt int64,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	lease, _, err := distributionLeaseForUpdate(ctx, tx, leaseID, owner, now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM distribution_leases WHERE id = ?`, leaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE distribution_requests
		 SET last_error = ?, next_attempt_at = ?, updated_at = ?
		 WHERE backend = ? AND track_ref = ?`,
		message, nextAttemptAt, now, lease.Backend, lease.TrackRef); err != nil {
		return err
	}
	if err := addDistributionMetricTx(ctx, tx, lease.Backend, "publish_failure", 1, now); err != nil {
		return err
	}
	return tx.Commit()
}

func distributionLeaseForUpdate(
	ctx context.Context,
	tx *sql.Tx,
	leaseID, owner string,
	now int64,
) (DistributionLease, int64, error) {
	var lease DistributionLease
	var requestedAt int64
	err := tx.QueryRowContext(ctx,
		`SELECT lease.id, lease.backend, lease.track_ref, lease.owner,
		        lease.expires_at, lease.created_at, request.requested_at
		 FROM distribution_leases AS lease
		 JOIN distribution_requests AS request
		   ON request.backend = lease.backend AND request.track_ref = lease.track_ref
		 WHERE lease.id = ? AND lease.owner = ?`, leaseID, owner).
		Scan(
			&lease.ID, &lease.Backend, &lease.TrackRef, &lease.Owner,
			&lease.ExpiresAt, &lease.CreatedAt, &requestedAt,
		)
	if errors.Is(err, sql.ErrNoRows) {
		return DistributionLease{}, 0, ErrDistributionLeaseInvalid
	}
	if err != nil {
		return DistributionLease{}, 0, err
	}
	if lease.ExpiresAt <= now {
		return DistributionLease{}, 0, ErrDistributionLeaseExpired
	}
	return lease, requestedAt, nil
}

func (s *Store) AddDistributionMetric(ctx context.Context, backend, name string, delta, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO distribution_metrics (backend, name, value, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(backend, name) DO UPDATE SET
		 value = distribution_metrics.value + excluded.value,
		 updated_at = excluded.updated_at`,
		backend, name, delta, now)
	return err
}

func addDistributionMetricTx(ctx context.Context, tx *sql.Tx, backend, name string, delta, now int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO distribution_metrics (backend, name, value, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(backend, name) DO UPDATE SET
		 value = distribution_metrics.value + excluded.value,
		 updated_at = excluded.updated_at`,
		backend, name, delta, now)
	return err
}

func (s *Store) DistributionMetrics(ctx context.Context, backend string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, value FROM distribution_metrics WHERE backend = ? ORDER BY name`, backend)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	metrics := make(map[string]int64)
	for rows.Next() {
		var name string
		var value int64
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		metrics[name] = value
	}
	return metrics, rows.Err()
}

func (s *Store) DistributionStatus(ctx context.Context, backend string, now int64) (DistributionStatus, error) {
	var status DistributionStatus
	err := s.db.QueryRowContext(ctx,
		`SELECT
		   COUNT(*),
		   COALESCE(SUM(CASE WHEN candidate.track_ref IS NULL AND lease.id IS NULL
		                 AND request.next_attempt_at <= ? THEN 1 ELSE 0 END),
		   0),
		   COALESCE(SUM(CASE WHEN lease.id IS NOT NULL THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN candidate.track_ref IS NOT NULL THEN 1 ELSE 0 END), 0)
		 FROM distribution_requests AS request
		 LEFT JOIN distribution_candidates AS candidate
		   ON candidate.backend = request.backend
		  AND candidate.track_ref = request.track_ref
		 LEFT JOIN distribution_leases AS lease
		   ON lease.backend = request.backend
		  AND lease.track_ref = request.track_ref
		  AND lease.expires_at > ?
		 WHERE request.backend = ?`, now, now, backend).
		Scan(&status.Requested, &status.Pending, &status.Leased, &status.Ready)
	return status, err
}
