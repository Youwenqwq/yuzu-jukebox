package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrAccelerationNotEmpty     = errors.New("acceleration has distribution state")
	ErrAccelerationNoCredential = errors.New("acceleration credential is not configured")
)

type Acceleration struct {
	ID                          string
	Name                        string
	Kind                        string
	Enabled                     bool
	PublishOnCacheReady         bool
	ControlBaseURL              string
	BackendBaseURL              string
	PublisherTokenHash          []byte
	PublisherPendingTokenHash   []byte
	DeliveryTokenHash           []byte
	DeliveryPendingTokenHash    []byte
	BackendToken                string
	BackendPendingToken         string
	LeaseTTLSeconds             int
	UploadRateBytesPerSecond    int64
	MaxObjectBytes              int64
	StorageBudgetBytes          int64
	StorageHighWatermarkPercent int
	StorageLowWatermarkPercent  int
	ControlHealthy              *bool
	BackendHealthy              *bool
	HealthError                 string
	LastHealthAt                *int64
	CreatedAt                   int64
	UpdatedAt                   int64
}

type AccelerationUpdate struct {
	Name                        string
	Enabled                     bool
	PublishOnCacheReady         bool
	ControlBaseURL              string
	BackendBaseURL              string
	LeaseTTLSeconds             int
	UploadRateBytesPerSecond    int64
	MaxObjectBytes              int64
	StorageBudgetBytes          int64
	StorageHighWatermarkPercent int
	StorageLowWatermarkPercent  int
	HealthChecked               bool
	ControlHealthy              bool
	BackendHealthy              bool
	HealthError                 string
	HealthCheckedAt             int64
}

type AccelerationPublisher struct {
	AccelerationID string
	Owner          string
	Version        string
	State          string
	LeaseID        string
	TrackRef       string
	Capabilities   string
	BackendHealthy bool
	LastError      string
	LastSeenAt     int64
}

type DistributionAttempt struct {
	LeaseID        string `json:"lease_id"`
	AccelerationID string `json:"acceleration_id"`
	TrackRef       string `json:"track_ref"`
	Owner          string `json:"owner"`
	Phase          string `json:"phase"`
	SourceBytes    int64  `json:"source_bytes"`
	UploadBytes    int64  `json:"upload_bytes"`
	TotalBytes     int64  `json:"total_bytes"`
	Status         string `json:"status"`
	LastError      string `json:"last_error,omitempty"`
	StartedAt      int64  `json:"started_at"`
	UpdatedAt      int64  `json:"updated_at"`
	FinishedAt     *int64 `json:"finished_at,omitempty"`
}

func (s *Store) CreateAcceleration(
	ctx context.Context,
	acceleration Acceleration,
	publisherHash, deliveryHash []byte,
	backendToken string,
) (Acceleration, error) {
	if acceleration.StorageBudgetBytes == 0 {
		acceleration.StorageBudgetBytes = 850 << 20
	}
	if acceleration.StorageHighWatermarkPercent == 0 {
		acceleration.StorageHighWatermarkPercent = 95
	}
	if acceleration.StorageLowWatermarkPercent == 0 {
		acceleration.StorageLowWatermarkPercent = 85
	}
	now := nowMs()
	encryptedBackend, err := s.encrypt(backendToken)
	if err != nil {
		return Acceleration{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO accelerations
		(id, name, kind, enabled, publish_on_cache_ready, control_base_url,
		 backend_base_url, publisher_token_hash, delivery_token_hash, backend_token,
		 lease_ttl_seconds, upload_rate_bytes_per_second, max_object_bytes,
		 storage_budget_bytes, storage_high_watermark_percent,
		 storage_low_watermark_percent, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		acceleration.ID, acceleration.Name, acceleration.Kind,
		acceleration.PublishOnCacheReady, acceleration.ControlBaseURL,
		acceleration.BackendBaseURL, publisherHash, deliveryHash, encryptedBackend,
		acceleration.LeaseTTLSeconds, acceleration.UploadRateBytesPerSecond,
		acceleration.MaxObjectBytes, acceleration.StorageBudgetBytes,
		acceleration.StorageHighWatermarkPercent,
		acceleration.StorageLowWatermarkPercent, now, now)
	if err != nil {
		return Acceleration{}, err
	}
	return s.GetAcceleration(ctx, acceleration.ID)
}

func (s *Store) GetAcceleration(ctx context.Context, id string) (Acceleration, error) {
	return s.scanAcceleration(s.db.QueryRowContext(ctx, accelerationSelect+` WHERE id = ?`, id))
}

func (s *Store) ListAccelerations(ctx context.Context) ([]Acceleration, error) {
	rows, err := s.db.QueryContext(ctx, accelerationSelect+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Acceleration, 0)
	for rows.Next() {
		acceleration, err := s.scanAcceleration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, acceleration)
	}
	return out, rows.Err()
}

func (s *Store) ListCacheReadyAccelerations(ctx context.Context) ([]Acceleration, error) {
	rows, err := s.db.QueryContext(ctx, accelerationSelect+
		` WHERE enabled = 1 AND publish_on_cache_ready = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Acceleration, 0)
	for rows.Next() {
		acceleration, err := s.scanAcceleration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, acceleration)
	}
	return out, rows.Err()
}

func (s *Store) UpdateAcceleration(ctx context.Context, id string, update AccelerationUpdate) (Acceleration, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Acceleration{}, err
	}
	defer tx.Rollback()
	now := nowMs()
	result, err := tx.ExecContext(ctx, `UPDATE accelerations SET
		name = ?, enabled = ?, publish_on_cache_ready = ?, control_base_url = ?,
		backend_base_url = ?, lease_ttl_seconds = ?, upload_rate_bytes_per_second = ?,
		max_object_bytes = ?, storage_budget_bytes = ?,
		storage_high_watermark_percent = ?, storage_low_watermark_percent = ?,
		control_healthy = CASE WHEN ? THEN ? ELSE control_healthy END,
		backend_healthy = CASE WHEN ? THEN ? ELSE backend_healthy END,
		health_error = CASE WHEN ? THEN ? ELSE health_error END,
		last_health_at = CASE WHEN ? THEN ? ELSE last_health_at END,
		updated_at = ? WHERE id = ?`,
		update.Name, update.Enabled, update.PublishOnCacheReady,
		update.ControlBaseURL, update.BackendBaseURL, update.LeaseTTLSeconds,
		update.UploadRateBytesPerSecond, update.MaxObjectBytes,
		update.StorageBudgetBytes, update.StorageHighWatermarkPercent,
		update.StorageLowWatermarkPercent,
		update.HealthChecked, update.ControlHealthy,
		update.HealthChecked, update.BackendHealthy,
		update.HealthChecked, update.HealthError,
		update.HealthChecked, update.HealthCheckedAt,
		now, id)
	if err != nil {
		return Acceleration{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Acceleration{}, err
	} else if affected == 0 {
		return Acceleration{}, sql.ErrNoRows
	}
	var accounted int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0)
		FROM acceleration_objects WHERE acceleration_id = ? AND state <> 'missing'`,
		id).Scan(&accounted); err != nil {
		return Acceleration{}, err
	}
	if update.StorageBudgetBytes > 0 &&
		accounted > update.StorageBudgetBytes*int64(update.StorageHighWatermarkPercent)/100 {
		if err := enqueueStorageGCTx(ctx, tx, id,
			update.StorageBudgetBytes*int64(update.StorageLowWatermarkPercent)/100,
			"", now); err != nil {
			return Acceleration{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Acceleration{}, err
	}
	return s.GetAcceleration(ctx, id)
}

func (s *Store) DeleteAcceleration(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT enabled FROM accelerations WHERE id = ?`, id).Scan(&enabled); err != nil {
		return err
	}
	if enabled {
		return fmt.Errorf("disable acceleration before deletion")
	}
	var state int64
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM distribution_leases WHERE acceleration_id = ?) +
		(SELECT COUNT(*) FROM distribution_candidates WHERE acceleration_id = ?) +
		(SELECT COUNT(*) FROM acceleration_objects WHERE acceleration_id = ?) +
		(SELECT COUNT(*) FROM acceleration_storage_reservations WHERE acceleration_id = ?) +
		(SELECT COUNT(*) FROM acceleration_deletion_jobs WHERE acceleration_id = ?) +
		(SELECT COUNT(*) FROM acceleration_inventory_snapshots WHERE acceleration_id = ?)`,
		id, id, id, id, id, id).Scan(&state); err != nil {
	}
	if state != 0 {
		return ErrAccelerationNotEmpty
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM accelerations WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResolveAccelerationPublisherToken(ctx context.Context, hash []byte) (Acceleration, error) {
	return s.scanAcceleration(s.db.QueryRowContext(ctx, accelerationSelect+
		` WHERE publisher_token_hash = ? OR publisher_pending_token_hash = ?`, hash, hash))
}

func (s *Store) ResolveAccelerationDeliveryToken(ctx context.Context, hash []byte) (Acceleration, error) {
	return s.scanAcceleration(s.db.QueryRowContext(ctx, accelerationSelect+
		` WHERE delivery_token_hash = ? OR delivery_pending_token_hash = ?`, hash, hash))
}

func (s *Store) PrepareAccelerationCredential(
	ctx context.Context,
	id, purpose string,
	hash []byte,
	secret string,
) (Acceleration, error) {
	var column string
	var value any
	switch purpose {
	case "publisher":
		column, value = "publisher_pending_token_hash", hash
	case "delivery":
		column, value = "delivery_pending_token_hash", hash
	case "backend":
		encrypted, err := s.encrypt(secret)
		if err != nil {
			return Acceleration{}, err
		}
		column, value = "backend_pending_token", encrypted
	default:
		return Acceleration{}, fmt.Errorf("unknown acceleration credential purpose %q", purpose)
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE accelerations SET `+column+` = ?, updated_at = ? WHERE id = ?`, value, nowMs(), id)
	if err != nil {
		return Acceleration{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Acceleration{}, err
	} else if affected == 0 {
		return Acceleration{}, sql.ErrNoRows
	}
	return s.GetAcceleration(ctx, id)
}

func (s *Store) ActivateAccelerationCredential(ctx context.Context, id, purpose string) (Acceleration, error) {
	var statement string
	switch purpose {
	case "publisher":
		statement = `UPDATE accelerations SET publisher_token_hash = publisher_pending_token_hash,
			publisher_pending_token_hash = NULL, updated_at = ?
			WHERE id = ? AND publisher_pending_token_hash IS NOT NULL`
	case "delivery":
		statement = `UPDATE accelerations SET delivery_token_hash = delivery_pending_token_hash,
			delivery_pending_token_hash = NULL, updated_at = ?
			WHERE id = ? AND delivery_pending_token_hash IS NOT NULL`
	case "backend":
		statement = `UPDATE accelerations SET backend_token = backend_pending_token,
			backend_pending_token = '', updated_at = ?
			WHERE id = ? AND backend_pending_token <> ''`
	default:
		return Acceleration{}, fmt.Errorf("unknown acceleration credential purpose %q", purpose)
	}
	result, err := s.db.ExecContext(ctx, statement, nowMs(), id)
	if err != nil {
		return Acceleration{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Acceleration{}, err
	} else if affected == 0 {
		return Acceleration{}, ErrAccelerationNoCredential
	}
	return s.GetAcceleration(ctx, id)
}

func (s *Store) UpdateAccelerationHealth(
	ctx context.Context,
	id string,
	controlHealthy, backendHealthy bool,
	healthError string,
	checkedAt int64,
) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accelerations SET
		control_healthy = ?, backend_healthy = ?, health_error = ?, last_health_at = ?
		WHERE id = ?`, controlHealthy, backendHealthy, healthError, checkedAt, id)
	return err
}

func (s *Store) UpsertAccelerationPublisher(ctx context.Context, publisher AccelerationPublisher) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO distribution_publishers
		(acceleration_id, owner, version, state, lease_id, track_ref, capabilities,
		 backend_healthy, last_error, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(acceleration_id, owner) DO UPDATE SET
		 version = excluded.version, state = excluded.state, lease_id = excluded.lease_id,
		 track_ref = excluded.track_ref, capabilities = excluded.capabilities,
		 backend_healthy = excluded.backend_healthy, last_error = excluded.last_error,
		 last_seen_at = excluded.last_seen_at`,
		publisher.AccelerationID, publisher.Owner, publisher.Version, publisher.State,
		publisher.LeaseID, publisher.TrackRef, publisher.Capabilities,
		publisher.BackendHealthy, publisher.LastError, publisher.LastSeenAt)
	return err
}

func (s *Store) ListAccelerationPublishers(ctx context.Context, accelerationID string) ([]AccelerationPublisher, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT acceleration_id, owner, version, state,
		lease_id, track_ref, capabilities, backend_healthy, last_error, last_seen_at
		FROM distribution_publishers WHERE acceleration_id = ? ORDER BY owner`, accelerationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AccelerationPublisher, 0)
	for rows.Next() {
		var publisher AccelerationPublisher
		if err := rows.Scan(&publisher.AccelerationID, &publisher.Owner, &publisher.Version,
			&publisher.State, &publisher.LeaseID, &publisher.TrackRef,
			&publisher.Capabilities, &publisher.BackendHealthy,
			&publisher.LastError, &publisher.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, publisher)
	}
	return out, rows.Err()
}

func (s *Store) AccelerationReady(ctx context.Context, id string, publisherCutoff int64) (bool, []string, error) {
	acceleration, err := s.GetAcceleration(ctx, id)
	if err != nil {
		return false, nil, err
	}
	return s.AccelerationReadyFor(ctx, acceleration, publisherCutoff)
}

func (s *Store) AccelerationReadyFor(
	ctx context.Context,
	acceleration Acceleration,
	publisherCutoff int64,
) (bool, []string, error) {
	problems := make([]string, 0, 6)
	if acceleration.ControlBaseURL == "" || acceleration.BackendBaseURL == "" {
		problems = append(problems, "endpoints_not_configured")
	}
	if len(acceleration.PublisherTokenHash) == 0 || len(acceleration.DeliveryTokenHash) == 0 || acceleration.BackendToken == "" {
		problems = append(problems, "credentials_not_configured")
	}
	if acceleration.StorageBudgetBytes <= 0 {
		problems = append(problems, "storage_unmanaged")
	}
	if acceleration.ControlHealthy == nil || !*acceleration.ControlHealthy ||
		acceleration.BackendHealthy == nil || !*acceleration.BackendHealthy {
		problems = append(problems, "health_check_failed")
	}
	var publishers, storageManagers int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN EXISTS (
		 SELECT 1 FROM json_each(distribution_publishers.capabilities)
		 WHERE value = 'storage.inventory'
		) AND EXISTS (
		 SELECT 1 FROM json_each(distribution_publishers.capabilities)
		 WHERE value = 'object.delete'
		) THEN 1 ELSE 0 END), 0)
		FROM distribution_publishers WHERE acceleration_id = ? AND last_seen_at >= ?`,
		acceleration.ID, publisherCutoff).Scan(&publishers, &storageManagers); err != nil {
		return false, nil, err
	}
	if publishers == 0 {
		problems = append(problems, "publisher_offline")
	} else if storageManagers == 0 {
		problems = append(problems, "storage_manager_offline")
	}
	return len(problems) == 0, problems, nil
}

const accelerationSelect = `SELECT id, name, kind, enabled, publish_on_cache_ready,
	control_base_url, backend_base_url, publisher_token_hash,
	publisher_pending_token_hash, delivery_token_hash, delivery_pending_token_hash,
	backend_token, backend_pending_token, lease_ttl_seconds,
	upload_rate_bytes_per_second, max_object_bytes, storage_budget_bytes,
	storage_high_watermark_percent, storage_low_watermark_percent, control_healthy,
	backend_healthy, health_error, last_health_at, created_at, updated_at
	FROM accelerations`

type accelerationScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanAcceleration(row accelerationScanner) (Acceleration, error) {
	var acceleration Acceleration
	var publisherHash, publisherPendingHash, deliveryHash, deliveryPendingHash []byte
	var encryptedBackend, encryptedPendingBackend string
	var controlHealthy, backendHealthy sql.NullBool
	var lastHealth sql.NullInt64
	err := row.Scan(
		&acceleration.ID, &acceleration.Name, &acceleration.Kind, &acceleration.Enabled,
		&acceleration.PublishOnCacheReady, &acceleration.ControlBaseURL,
		&acceleration.BackendBaseURL, &publisherHash, &publisherPendingHash,
		&deliveryHash, &deliveryPendingHash, &encryptedBackend, &encryptedPendingBackend,
		&acceleration.LeaseTTLSeconds, &acceleration.UploadRateBytesPerSecond,
		&acceleration.MaxObjectBytes, &acceleration.StorageBudgetBytes,
		&acceleration.StorageHighWatermarkPercent,
		&acceleration.StorageLowWatermarkPercent, &controlHealthy, &backendHealthy,
		&acceleration.HealthError, &lastHealth, &acceleration.CreatedAt,
		&acceleration.UpdatedAt,
	)
	if err != nil {
		return Acceleration{}, err
	}
	acceleration.PublisherTokenHash = publisherHash
	acceleration.PublisherPendingTokenHash = publisherPendingHash
	acceleration.DeliveryTokenHash = deliveryHash
	acceleration.DeliveryPendingTokenHash = deliveryPendingHash
	if acceleration.BackendToken, err = s.decrypt(encryptedBackend); err != nil {
		return Acceleration{}, err
	}
	if acceleration.BackendPendingToken, err = s.decrypt(encryptedPendingBackend); err != nil {
		return Acceleration{}, err
	}
	if controlHealthy.Valid {
		value := controlHealthy.Bool
		acceleration.ControlHealthy = &value
	}
	if backendHealthy.Valid {
		value := backendHealthy.Bool
		acceleration.BackendHealthy = &value
	}
	if lastHealth.Valid {
		value := lastHealth.Int64
		acceleration.LastHealthAt = &value
	}
	return acceleration, nil
}

func accelerationOnlineCutoff(now time.Time) int64 {
	return now.Add(-45 * time.Second).UnixMilli()
}
