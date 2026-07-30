package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrDistributionLeaseInvalid          = errors.New("distribution lease invalid")
	ErrDistributionLeaseExpired          = errors.New("distribution lease expired")
	ErrDistributionProgressStale         = errors.New("distribution progress is stale")
	ErrDistributionCancellationRequested = errors.New("distribution cancellation requested")
	ErrDistributionRequestReady          = errors.New("distribution request is ready")
)

type DistributionLease struct {
	ID              string `json:"id"`
	AccelerationID  string `json:"acceleration_id"`
	TrackRef        string `json:"track_ref"`
	Owner           string `json:"owner"`
	ExpiresAt       int64  `json:"expires_at"`
	CreatedAt       int64  `json:"created_at"`
	CancelRequested bool   `json:"cancel_requested,omitempty"`
}

type DistributionCandidate struct {
	AccelerationID string `json:"acceleration_id"`
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
	Requested       int64 `json:"requested"`
	Queued          int64 `json:"queued"`
	Leased          int64 `json:"leased"`
	RetryWait       int64 `json:"retry_wait"`
	CancelRequested int64 `json:"cancel_requested"`
	Ready           int64 `json:"ready"`
	Canceled        int64 `json:"canceled"`
	OldestQueuedAt  int64 `json:"oldest_queued_at,omitempty"`
}

type DistributionRequestView struct {
	AccelerationID    string                 `json:"acceleration_id"`
	TrackRef          string                 `json:"track_ref"`
	State             string                 `json:"state"`
	PendingReason     string                 `json:"pending_reason,omitempty"`
	RequestedAt       int64                  `json:"requested_at"`
	UpdatedAt         int64                  `json:"updated_at"`
	NextAttemptAt     int64                  `json:"next_attempt_at"`
	Attempts          int64                  `json:"attempts"`
	LastError         string                 `json:"last_error,omitempty"`
	CancelRequestedAt int64                  `json:"cancel_requested_at,omitempty"`
	CanceledAt        int64                  `json:"canceled_at,omitempty"`
	Lease             *DistributionLease     `json:"lease,omitempty"`
	Candidate         *DistributionCandidate `json:"candidate,omitempty"`
	Progress          *DistributionAttempt   `json:"progress,omitempty"`
}

// RequestDistribution records demand for a track. Repeated cache notifications
// and edge introspection requests do not create duplicate work.
func (s *Store) RequestDistribution(ctx context.Context, accelerationID, trackRef string, now int64) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO distribution_requests
		(acceleration_id, track_ref, requested_at, updated_at, next_attempt_at)
		VALUES (?, ?, ?, ?, 0)
		ON CONFLICT(acceleration_id, track_ref) DO NOTHING`,
		accelerationID, trackRef, now, now)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		_, err = s.db.ExecContext(ctx, `UPDATE distribution_requests SET
			updated_at = ?, next_attempt_at = CASE WHEN canceled_at > 0 THEN 0 ELSE next_attempt_at END,
			last_error = CASE WHEN canceled_at > 0 THEN '' ELSE last_error END,
			cancel_requested_at = CASE WHEN canceled_at > 0 THEN 0 ELSE cancel_requested_at END,
			canceled_at = 0
			WHERE acceleration_id = ? AND track_ref = ?`, now, accelerationID, trackRef)
		return err
	}
	return s.AddDistributionMetric(ctx, accelerationID, "requests", 1, now)
}

func (s *Store) GetDistributionCandidate(ctx context.Context, accelerationID, trackRef string) (DistributionCandidate, error) {
	var candidate DistributionCandidate
	err := s.db.QueryRowContext(ctx, `SELECT acceleration_id, track_ref,
		content_version, locator, layout, size_bytes, content_type, etag,
		created_at, updated_at FROM distribution_candidates
		WHERE acceleration_id = ? AND track_ref = ?`, accelerationID, trackRef).Scan(
		&candidate.AccelerationID, &candidate.TrackRef, &candidate.ContentVersion,
		&candidate.Locator, &candidate.Layout, &candidate.SizeBytes,
		&candidate.ContentType, &candidate.ETag, &candidate.CreatedAt,
		&candidate.UpdatedAt,
	)
	return candidate, err
}

// ClaimDistribution leases the oldest requested track that has no ready
// candidate and no live publisher. Expired leases are reclaimed atomically.
func (s *Store) ClaimDistribution(
	ctx context.Context,
	accelerationID, owner, leaseID string,
	now, expiresAt int64,
) (DistributionLease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DistributionLease{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE distribution_attempts SET
		status = 'canceled', last_error = 'canceled by administrator',
		finished_at = ?, updated_at = ? WHERE status = 'active' AND lease_id IN
		(SELECT lease.id FROM distribution_leases lease
		 JOIN distribution_requests request
		  ON request.acceleration_id = lease.acceleration_id
		  AND request.track_ref = lease.track_ref
		 WHERE lease.expires_at <= ? AND request.cancel_requested_at > 0)`,
		now, now, now); err != nil {
		return DistributionLease{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_attempts SET
		status = 'expired', last_error = 'lease expired', finished_at = ?, updated_at = ?
		WHERE status = 'active' AND lease_id IN
		(SELECT lease.id FROM distribution_leases lease
		 JOIN distribution_requests request
		  ON request.acceleration_id = lease.acceleration_id
		  AND request.track_ref = lease.track_ref
		 WHERE lease.expires_at <= ? AND request.cancel_requested_at = 0)`,
		now, now, now); err != nil {
		return DistributionLease{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_requests SET
		canceled_at = ?, updated_at = ? WHERE cancel_requested_at > 0 AND EXISTS
		(SELECT 1 FROM distribution_leases lease
		 WHERE lease.acceleration_id = distribution_requests.acceleration_id
		 AND lease.track_ref = distribution_requests.track_ref AND lease.expires_at <= ?)`,
		now, now, now); err != nil {
		return DistributionLease{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM distribution_leases WHERE expires_at <= ?`, now); err != nil {
		return DistributionLease{}, err
	}

	var trackRef string
	err = tx.QueryRowContext(ctx, `SELECT request.track_ref
		FROM distribution_requests AS request
		LEFT JOIN distribution_candidates AS candidate
		 ON candidate.acceleration_id = request.acceleration_id
		 AND candidate.track_ref = request.track_ref
		LEFT JOIN distribution_leases AS lease
		 ON lease.acceleration_id = request.acceleration_id
		 AND lease.track_ref = request.track_ref
		WHERE request.acceleration_id = ? AND request.next_attempt_at <= ?
		 AND request.cancel_requested_at = 0 AND request.canceled_at = 0
		 AND candidate.track_ref IS NULL AND lease.id IS NULL
		ORDER BY request.requested_at, request.track_ref LIMIT 1`, accelerationID, now).Scan(&trackRef)
	if err != nil {
		return DistributionLease{}, err
	}

	lease := DistributionLease{
		ID: leaseID, AccelerationID: accelerationID, TrackRef: trackRef,
		Owner: owner, ExpiresAt: expiresAt, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO distribution_leases
		(id, acceleration_id, track_ref, owner, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, lease.ID, lease.AccelerationID,
		lease.TrackRef, lease.Owner, lease.ExpiresAt, lease.CreatedAt); err != nil {
		return DistributionLease{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO distribution_attempts
		(lease_id, acceleration_id, track_ref, owner, phase, status,
		 started_at, updated_at) VALUES (?, ?, ?, ?, 'claimed', 'active', ?, ?)`,
		lease.ID, lease.AccelerationID, lease.TrackRef, lease.Owner, now, now); err != nil {
		return DistributionLease{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_requests
		SET attempts = attempts + 1, updated_at = ?
		WHERE acceleration_id = ? AND track_ref = ?`, now, accelerationID, trackRef); err != nil {
		return DistributionLease{}, err
	}
	if err := addDistributionMetricTx(ctx, tx, accelerationID, "leases", 1, now); err != nil {
		return DistributionLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return DistributionLease{}, err
	}
	return lease, nil
}

func (s *Store) GetDistributionLease(ctx context.Context, leaseID string) (DistributionLease, error) {
	var lease DistributionLease
	err := s.db.QueryRowContext(ctx, `SELECT lease.id, lease.acceleration_id,
		lease.track_ref, lease.owner, lease.expires_at, lease.created_at,
		request.cancel_requested_at > 0
		FROM distribution_leases lease JOIN distribution_requests request
		 ON request.acceleration_id = lease.acceleration_id
		 AND request.track_ref = lease.track_ref
		WHERE lease.id = ?`, leaseID).Scan(
		&lease.ID, &lease.AccelerationID, &lease.TrackRef, &lease.Owner,
		&lease.ExpiresAt, &lease.CreatedAt, &lease.CancelRequested,
	)
	return lease, err
}
func (s *Store) RequestDistributionCancellation(
	ctx context.Context,
	accelerationID, trackRef string,
	now int64,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var canceledAt int64
	var candidate, lease sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT request.canceled_at, candidate.track_ref, lease.id
		FROM distribution_requests request
		LEFT JOIN distribution_candidates candidate
		 ON candidate.acceleration_id = request.acceleration_id
		 AND candidate.track_ref = request.track_ref
		LEFT JOIN distribution_leases lease
		 ON lease.acceleration_id = request.acceleration_id
		 AND lease.track_ref = request.track_ref AND lease.expires_at > ?
		WHERE request.acceleration_id = ? AND request.track_ref = ?`,
		now, accelerationID, trackRef).Scan(&canceledAt, &candidate, &lease)
	if err != nil {
		return err
	}
	if canceledAt > 0 {
		return tx.Commit()
	}
	if candidate.Valid {
		return ErrDistributionRequestReady
	}
	if lease.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE distribution_requests SET
			cancel_requested_at = CASE WHEN cancel_requested_at = 0 THEN ? ELSE cancel_requested_at END,
			last_error = 'cancellation requested by administrator', updated_at = ?
			WHERE acceleration_id = ? AND track_ref = ?`,
			now, now, accelerationID, trackRef); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_requests SET
		cancel_requested_at = ?, canceled_at = ?, next_attempt_at = 0,
		last_error = 'canceled by administrator', updated_at = ?
		WHERE acceleration_id = ? AND track_ref = ?`,
		now, now, now, accelerationID, trackRef); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CompleteDistributionCancellation(
	ctx context.Context,
	leaseID, owner string,
	now int64,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var accelerationID, trackRef string
	var cancelRequestedAt int64
	err = tx.QueryRowContext(ctx, `SELECT lease.acceleration_id, lease.track_ref,
		request.cancel_requested_at FROM distribution_leases lease
		JOIN distribution_requests request
		 ON request.acceleration_id = lease.acceleration_id
		 AND request.track_ref = lease.track_ref
		WHERE lease.id = ? AND lease.owner = ?`, leaseID, owner).Scan(
		&accelerationID, &trackRef, &cancelRequestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDistributionLeaseInvalid
	}
	if err != nil {
		return err
	}
	if cancelRequestedAt == 0 {
		return ErrDistributionLeaseInvalid
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_attempts SET
		status = 'canceled', last_error = 'canceled by administrator',
		updated_at = ?, finished_at = ? WHERE lease_id = ? AND status = 'active'`,
		now, now, leaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM distribution_leases WHERE id = ?`, leaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_requests SET
		canceled_at = ?, next_attempt_at = 0, last_error = 'canceled by administrator',
		updated_at = ? WHERE acceleration_id = ? AND track_ref = ?`,
		now, now, accelerationID, trackRef); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateDistributionProgress(
	ctx context.Context,
	leaseID, owner, phase string,
	sourceBytes, uploadBytes, totalBytes, now, expiresAt int64,
) (DistributionLease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DistributionLease{}, err
	}
	defer tx.Rollback()
	lease, _, err := distributionLeaseForUpdate(ctx, tx, leaseID, owner, now)
	if err != nil {
		return DistributionLease{}, err
	}
	var currentPhase string
	var currentSource, currentUpload, currentTotal int64
	if err := tx.QueryRowContext(ctx, `SELECT phase, source_bytes, upload_bytes,
		total_bytes FROM distribution_attempts WHERE lease_id = ? AND status = 'active'`, leaseID).Scan(
		&currentPhase, &currentSource, &currentUpload, &currentTotal,
	); err != nil {
		return DistributionLease{}, err
	}
	if !validProgressTransition(currentPhase, phase) || sourceBytes < currentSource ||
		uploadBytes < currentUpload || totalBytes < currentTotal {
		return DistributionLease{}, ErrDistributionProgressStale
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_attempts SET
		phase = ?, source_bytes = ?, upload_bytes = ?, total_bytes = ?, updated_at = ?
		WHERE lease_id = ?`, phase, sourceBytes, uploadBytes, totalBytes, now, leaseID); err != nil {
		return DistributionLease{}, err
	}
	if expiresAt > lease.ExpiresAt {
		if _, err := tx.ExecContext(ctx, `UPDATE distribution_leases SET expires_at = ? WHERE id = ?`, expiresAt, leaseID); err != nil {
			return DistributionLease{}, err
		}
		lease.ExpiresAt = expiresAt
	}
	if err := tx.Commit(); err != nil {
		return DistributionLease{}, err
	}
	return lease, nil
}

func validProgressTransition(current, next string) bool {
	rank := map[string]int{"claimed": 0, "downloading": 1, "uploading": 2, "verifying": 3, "completing": 4}
	currentRank, currentOK := rank[current]
	nextRank, nextOK := rank[next]
	return currentOK && nextOK && nextRank >= currentRank
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
	if candidate.AccelerationID != lease.AccelerationID || candidate.TrackRef != lease.TrackRef {
		return fmt.Errorf("%w: candidate does not match lease", ErrDistributionLeaseInvalid)
	}
	if candidate.Layout == "" {
		candidate.Layout = "object"
	}
	candidate.Locator = strings.TrimSpace(candidate.Locator)
	if candidate.Locator == "" || candidate.SizeBytes <= 0 {
		return fmt.Errorf("%w: invalid candidate object", ErrDistributionLeaseInvalid)
	}
	var reservationSize int64
	reservationErr := tx.QueryRowContext(ctx, `SELECT size_bytes
		FROM acceleration_storage_reservations WHERE lease_id = ?
		AND acceleration_id = ? AND locator = ? AND expires_at > ?`,
		leaseID, lease.AccelerationID, candidate.Locator, now).Scan(&reservationSize)
	uploaded := reservationErr == nil
	if reservationErr != nil {
		if !errors.Is(reservationErr, sql.ErrNoRows) {
			return reservationErr
		}
		var existingSize int64
		if err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM acceleration_objects
			WHERE acceleration_id = ? AND locator = ? AND state IN ('ready', 'orphan')`,
			lease.AccelerationID, candidate.Locator).Scan(&existingSize); err != nil {
			return fmt.Errorf("%w: object was not reserved", ErrDistributionLeaseInvalid)
		}
		reservationSize = existingSize
	}
	if reservationSize != candidate.SizeBytes {
		return fmt.Errorf("%w: object size does not match reservation", ErrDistributionLeaseInvalid)
	}
	var previousLocator sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT locator FROM distribution_candidates
		WHERE acceleration_id = ? AND track_ref = ?`, lease.AccelerationID,
		lease.TrackRef).Scan(&previousLocator); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO acceleration_objects
		(acceleration_id, locator, content_version, size_bytes, external_version,
		 state, reference_count, last_accessed_at, last_observed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'ready', 0, ?, 0, ?, ?)
		ON CONFLICT(acceleration_id, locator) DO UPDATE SET
		 content_version = excluded.content_version, size_bytes = excluded.size_bytes,
		 external_version = excluded.external_version, state = 'ready',
		 last_accessed_at = MAX(acceleration_objects.last_accessed_at, excluded.last_accessed_at),
		 updated_at = excluded.updated_at`,
		lease.AccelerationID, candidate.Locator, candidate.ContentVersion,
		candidate.SizeBytes, candidate.ETag, now, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO distribution_candidates
		(acceleration_id, track_ref, content_version, locator, layout, size_bytes,
		 content_type, etag, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(acceleration_id, track_ref) DO UPDATE SET
		 content_version = excluded.content_version, locator = excluded.locator,
		 layout = excluded.layout, size_bytes = excluded.size_bytes,
		 content_type = excluded.content_type, etag = excluded.etag,
		 updated_at = excluded.updated_at`, candidate.AccelerationID,
		candidate.TrackRef, candidate.ContentVersion, candidate.Locator,
		candidate.Layout, candidate.SizeBytes, candidate.ContentType,
		candidate.ETag, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE acceleration_objects SET
		reference_count = (SELECT COUNT(*) FROM distribution_candidates
		 WHERE acceleration_id = ? AND locator = acceleration_objects.locator),
		updated_at = ? WHERE acceleration_id = ? AND locator IN (?, ?)`,
		lease.AccelerationID, now, lease.AccelerationID, candidate.Locator,
		previousLocator.String); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_attempts SET
		phase = 'completing', status = 'succeeded', updated_at = ?, finished_at = ?
		WHERE lease_id = ?`, now, now, leaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM distribution_leases WHERE id = ?`, leaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_requests SET
		last_error = '', next_attempt_at = 0, updated_at = ?
		WHERE acceleration_id = ? AND track_ref = ?`, now,
		lease.AccelerationID, lease.TrackRef); err != nil {
		return err
	}
	metrics := map[string]int64{
		"publish_success":        1,
		"ready_latency_ms_total": max(now-requestedAt, 0), "ready_latency_samples": 1,
	}
	if uploaded {
		metrics["uploaded_bytes"] = candidate.SizeBytes
	}
	for name, delta := range metrics {
		if err := addDistributionMetricTx(ctx, tx, lease.AccelerationID, name, delta, now); err != nil {
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
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_attempts SET
		status = 'failed', last_error = ?, updated_at = ?, finished_at = ?
		WHERE lease_id = ?`, message, now, now, leaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM distribution_leases WHERE id = ?`, leaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE distribution_requests SET
		last_error = ?, next_attempt_at = ?, updated_at = ?
		WHERE acceleration_id = ? AND track_ref = ?`, message, nextAttemptAt,
		now, lease.AccelerationID, lease.TrackRef); err != nil {
		return err
	}
	if err := addDistributionMetricTx(ctx, tx, lease.AccelerationID, "publish_failure", 1, now); err != nil {
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
	var requestedAt, cancelRequestedAt int64
	err := tx.QueryRowContext(ctx, `SELECT lease.id, lease.acceleration_id,
		lease.track_ref, lease.owner, lease.expires_at, lease.created_at,
		request.requested_at, request.cancel_requested_at
		FROM distribution_leases AS lease
		JOIN distribution_requests AS request
		 ON request.acceleration_id = lease.acceleration_id
		 AND request.track_ref = lease.track_ref
		WHERE lease.id = ? AND lease.owner = ?`, leaseID, owner).Scan(
		&lease.ID, &lease.AccelerationID, &lease.TrackRef, &lease.Owner,
		&lease.ExpiresAt, &lease.CreatedAt, &requestedAt, &cancelRequestedAt,
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
	if cancelRequestedAt > 0 {
		lease.CancelRequested = true
		return DistributionLease{}, 0, ErrDistributionCancellationRequested
	}
	return lease, requestedAt, nil
}

func (s *Store) AddDistributionMetric(ctx context.Context, accelerationID, name string, delta, now int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := addDistributionMetricTx(ctx, tx, accelerationID, name, delta, now); err != nil {
		return err
	}
	return tx.Commit()
}

func addDistributionMetricTx(ctx context.Context, tx *sql.Tx, accelerationID, name string, delta, now int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO distribution_metrics
		(acceleration_id, name, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(acceleration_id, name) DO UPDATE SET
		 value = distribution_metrics.value + excluded.value,
		 updated_at = excluded.updated_at`, accelerationID, name, delta, now); err != nil {
		return err
	}
	bucket := now - now%(60*60*1000)
	_, err := tx.ExecContext(ctx, `INSERT INTO distribution_metric_buckets
		(acceleration_id, bucket_start, name, value) VALUES (?, ?, ?, ?)
		ON CONFLICT(acceleration_id, bucket_start, name) DO UPDATE SET
		 value = distribution_metric_buckets.value + excluded.value`,
		accelerationID, bucket, name, delta)
	return err
}

func (s *Store) DistributionMetrics(ctx context.Context, accelerationID string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, value FROM distribution_metrics
		WHERE acceleration_id = ? ORDER BY name`, accelerationID)
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

func (s *Store) DistributionMetricsSince(ctx context.Context, accelerationID string, since int64) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, SUM(value)
		FROM distribution_metric_buckets WHERE acceleration_id = ? AND bucket_start >= ?
		GROUP BY name ORDER BY name`, accelerationID, since)
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

func (s *Store) DistributionStatus(ctx context.Context, accelerationID string, now int64) (DistributionStatus, error) {
	var status DistributionStatus
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN request.canceled_at = 0 AND request.cancel_requested_at = 0
		 AND candidate.track_ref IS NULL AND lease.id IS NULL
		 AND request.next_attempt_at <= ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN request.cancel_requested_at = 0
		 AND request.canceled_at = 0 AND lease.id IS NOT NULL THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN request.canceled_at = 0 AND request.cancel_requested_at = 0
		 AND candidate.track_ref IS NULL AND lease.id IS NULL
		 AND request.next_attempt_at > ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN request.canceled_at = 0
		 AND request.cancel_requested_at > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN request.canceled_at = 0
		 AND candidate.track_ref IS NOT NULL THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN request.canceled_at > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(MIN(CASE WHEN request.canceled_at = 0 AND request.cancel_requested_at = 0
		 AND candidate.track_ref IS NULL AND lease.id IS NULL
		 AND request.next_attempt_at <= ? THEN request.requested_at END), 0)
		FROM distribution_requests AS request
		LEFT JOIN distribution_candidates AS candidate
		 ON candidate.acceleration_id = request.acceleration_id
		 AND candidate.track_ref = request.track_ref
		LEFT JOIN distribution_leases AS lease
		 ON lease.acceleration_id = request.acceleration_id
		 AND lease.track_ref = request.track_ref AND lease.expires_at > ?
		WHERE request.acceleration_id = ?`, now, now, now, now, accelerationID).Scan(
		&status.Requested, &status.Queued, &status.Leased, &status.RetryWait,
		&status.CancelRequested, &status.Ready, &status.Canceled, &status.OldestQueuedAt,
	)
	return status, err
}

func (s *Store) ListDistributionAttempts(ctx context.Context, accelerationID, status string, limit int) ([]DistributionAttempt, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT lease_id, acceleration_id, track_ref, owner, phase,
		source_bytes, upload_bytes, total_bytes, status, last_error,
		started_at, updated_at, finished_at FROM distribution_attempts
		WHERE acceleration_id = ?`
	args := []any{accelerationID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DistributionAttempt, 0)
	for rows.Next() {
		attempt, err := scanDistributionAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, attempt)
	}
	return out, rows.Err()
}

func (s *Store) ListDistributionRequests(ctx context.Context, accelerationID, state string, now int64, limit int) ([]DistributionRequestView, error) {
	return s.loadDistributionRequests(ctx, accelerationID, "", state, now, limit)
}

func (s *Store) GetDistributionRequest(
	ctx context.Context,
	accelerationID, trackRef string,
	now int64,
) (DistributionRequestView, error) {
	rows, err := s.loadDistributionRequests(ctx, accelerationID, trackRef, "", now, 1)
	if err != nil {
		return DistributionRequestView{}, err
	}
	if len(rows) == 0 {
		return DistributionRequestView{}, sql.ErrNoRows
	}
	return rows[0], nil
}

func (s *Store) loadDistributionRequests(
	ctx context.Context,
	accelerationID, trackRef, state string,
	now int64,
	limit int,
) ([]DistributionRequestView, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	state = strings.TrimSpace(state)
	query := `SELECT request.track_ref, request.requested_at, request.updated_at,
		request.next_attempt_at, request.attempts, request.last_error,
		request.cancel_requested_at, request.canceled_at,
		lease.id, lease.owner, lease.expires_at, lease.created_at,
		candidate.content_version, candidate.locator, candidate.layout,
		candidate.size_bytes, candidate.content_type, candidate.etag,
		candidate.created_at, candidate.updated_at,
		attempt.phase, attempt.source_bytes, attempt.upload_bytes,
		attempt.total_bytes, attempt.updated_at,
		EXISTS (SELECT 1 FROM distribution_publishers publisher
		 WHERE publisher.acceleration_id = request.acceleration_id
		 AND publisher.last_seen_at >= ?)
		FROM distribution_requests AS request
		LEFT JOIN distribution_leases AS lease
		 ON lease.acceleration_id = request.acceleration_id
		 AND lease.track_ref = request.track_ref AND lease.expires_at > ?
		LEFT JOIN distribution_candidates AS candidate
		 ON candidate.acceleration_id = request.acceleration_id
		 AND candidate.track_ref = request.track_ref
		LEFT JOIN distribution_attempts AS attempt
		 ON attempt.lease_id = lease.id AND attempt.status = 'active'
		WHERE request.acceleration_id = ?`
	args := []any{now - 45_000, now, accelerationID}
	if trackRef != "" {
		query += ` AND request.track_ref = ?`
		args = append(args, trackRef)
	}
	switch state {
	case "", "all":
	case "queued":
		query += ` AND request.canceled_at = 0 AND request.cancel_requested_at = 0
		 AND candidate.track_ref IS NULL AND lease.id IS NULL AND request.next_attempt_at <= ?`
		args = append(args, now)
	case "leased":
		query += ` AND request.canceled_at = 0 AND request.cancel_requested_at = 0 AND lease.id IS NOT NULL`
	case "retry_wait":
		query += ` AND request.canceled_at = 0 AND request.cancel_requested_at = 0
		 AND candidate.track_ref IS NULL AND lease.id IS NULL AND request.next_attempt_at > ?`
		args = append(args, now)
	case "cancel_requested":
		query += ` AND request.canceled_at = 0 AND request.cancel_requested_at > 0`
	case "ready":
		query += ` AND request.canceled_at = 0 AND candidate.track_ref IS NOT NULL`
	case "canceled":
		query += ` AND request.canceled_at > 0`
	default:
		return nil, fmt.Errorf("invalid distribution request state %q", state)
	}
	query += ` ORDER BY request.updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DistributionRequestView, 0)
	for rows.Next() {
		var view DistributionRequestView
		view.AccelerationID = accelerationID
		var leaseID, owner sql.NullString
		var leaseExpires, leaseCreated sql.NullInt64
		var contentVersion, locator, layout, contentType, etag sql.NullString
		var sizeBytes, candidateCreated, candidateUpdated sql.NullInt64
		var phase sql.NullString
		var sourceBytes, uploadBytes, totalBytes, progressUpdated sql.NullInt64
		var publisherOnline bool
		if err := rows.Scan(&view.TrackRef, &view.RequestedAt, &view.UpdatedAt,
			&view.NextAttemptAt, &view.Attempts, &view.LastError,
			&view.CancelRequestedAt, &view.CanceledAt,
			&leaseID, &owner, &leaseExpires, &leaseCreated,
			&contentVersion, &locator, &layout, &sizeBytes, &contentType, &etag,
			&candidateCreated, &candidateUpdated, &phase, &sourceBytes,
			&uploadBytes, &totalBytes, &progressUpdated, &publisherOnline); err != nil {
			return nil, err
		}
		switch {
		case view.CanceledAt > 0:
			view.State = "canceled"
		case contentVersion.Valid:
			view.State = "ready"
			view.Candidate = &DistributionCandidate{
				AccelerationID: accelerationID, TrackRef: view.TrackRef,
				ContentVersion: contentVersion.String, Locator: locator.String,
				Layout: layout.String, SizeBytes: sizeBytes.Int64,
				ContentType: contentType.String, ETag: etag.String,
				CreatedAt: candidateCreated.Int64, UpdatedAt: candidateUpdated.Int64,
			}
		case leaseID.Valid:
			if view.CancelRequestedAt > 0 {
				view.State = "cancel_requested"
			} else {
				view.State = "leased"
			}
			view.Lease = &DistributionLease{ID: leaseID.String, AccelerationID: accelerationID,
				TrackRef: view.TrackRef, Owner: owner.String, ExpiresAt: leaseExpires.Int64,
				CreatedAt: leaseCreated.Int64, CancelRequested: view.CancelRequestedAt > 0}
			if phase.Valid {
				view.Progress = &DistributionAttempt{LeaseID: leaseID.String,
					AccelerationID: accelerationID, TrackRef: view.TrackRef,
					Owner: owner.String, Phase: phase.String, SourceBytes: sourceBytes.Int64,
					UploadBytes: uploadBytes.Int64, TotalBytes: totalBytes.Int64,
					Status: "active", StartedAt: leaseCreated.Int64,
					UpdatedAt: progressUpdated.Int64}
			}
		case view.NextAttemptAt > now:
			view.State = "retry_wait"
			view.PendingReason = "retry_backoff"
		default:
			view.State = "queued"
			switch {
			case !publisherOnline:
				view.PendingReason = "publisher_offline"
			case view.LastError != "":
				view.PendingReason = "retry_due"
			default:
				view.PendingReason = "awaiting_claim"
			}
		}
		out = append(out, view)
	}
	return out, rows.Err()
}

type distributionAttemptScanner interface {
	Scan(dest ...any) error
}

func scanDistributionAttempt(row distributionAttemptScanner) (DistributionAttempt, error) {
	var attempt DistributionAttempt
	var finished sql.NullInt64
	err := row.Scan(&attempt.LeaseID, &attempt.AccelerationID, &attempt.TrackRef,
		&attempt.Owner, &attempt.Phase, &attempt.SourceBytes, &attempt.UploadBytes,
		&attempt.TotalBytes, &attempt.Status, &attempt.LastError,
		&attempt.StartedAt, &attempt.UpdatedAt, &finished)
	if finished.Valid {
		value := finished.Int64
		attempt.FinishedAt = &value
	}
	return attempt, err
}
