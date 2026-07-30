package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrAccelerationInventoryScanInvalid = errors.New("acceleration inventory scan invalid")

type AccelerationInventoryScan struct {
	ID             string `json:"id"`
	AccelerationID string `json:"acceleration_id"`
	Owner          string `json:"owner,omitempty"`
	State          string `json:"state"`
	Attempts       int64  `json:"attempts"`
	LeaseExpiresAt int64  `json:"lease_expires_at,omitempty"`
	ObservedAt     int64  `json:"observed_at,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	RequestedAt    int64  `json:"requested_at"`
	StartedAt      int64  `json:"started_at,omitempty"`
	CompletedAt    int64  `json:"completed_at,omitempty"`
	UpdatedAt      int64  `json:"updated_at"`
}

func (s *Store) RequestAccelerationInventoryScan(
	ctx context.Context,
	accelerationID string,
	now int64,
) (AccelerationInventoryScan, error) {
	if scan, err := s.activeAccelerationInventoryScan(ctx, accelerationID); err == nil {
		return scan, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return AccelerationInventoryScan{}, err
	}
	id, err := randomStorageID("inv_")
	if err != nil {
		return AccelerationInventoryScan{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO acceleration_inventory_scans
		(id, acceleration_id, state, requested_at, updated_at)
		VALUES (?, ?, 'queued', ?, ?)`, id, accelerationID, now, now)
	if err != nil {
		if scan, activeErr := s.activeAccelerationInventoryScan(ctx, accelerationID); activeErr == nil {
			return scan, nil
		}
		return AccelerationInventoryScan{}, err
	}
	return s.GetAccelerationInventoryScan(ctx, accelerationID, id)
}

func (s *Store) ClaimAccelerationInventoryScan(
	ctx context.Context,
	accelerationID, owner string,
	ttl time.Duration,
	now int64,
) (AccelerationInventoryScan, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return AccelerationInventoryScan{}, ErrAccelerationInventoryScanInvalid
	}
	if ttl <= 0 || ttl > 30*time.Minute {
		ttl = 30 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AccelerationInventoryScan{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE acceleration_inventory_scans SET
		state = 'queued', owner = '', lease_expires_at = 0, observed_at = 0,
		last_error = 'inventory lease expired', updated_at = ?
		WHERE acceleration_id = ? AND state = 'leased' AND lease_expires_at <= ?`,
		now, accelerationID, now); err != nil {
		return AccelerationInventoryScan{}, err
	}
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM acceleration_inventory_scans
		WHERE acceleration_id = ? AND state = 'queued'
		ORDER BY requested_at, id LIMIT 1`, accelerationID).Scan(&id); err != nil {
		return AccelerationInventoryScan{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM acceleration_inventory_snapshots
		WHERE id = ?`, id); err != nil {
		return AccelerationInventoryScan{}, err
	}
	expiresAt := now + ttl.Milliseconds()
	if _, err := tx.ExecContext(ctx, `UPDATE acceleration_inventory_scans SET
		state = 'leased', owner = ?, attempts = attempts + 1,
		lease_expires_at = ?, observed_at = 0, last_error = '',
		started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END, updated_at = ?
		WHERE id = ?`, owner, expiresAt, now, now, id); err != nil {
		return AccelerationInventoryScan{}, err
	}
	if err := tx.Commit(); err != nil {
		return AccelerationInventoryScan{}, err
	}
	return s.GetAccelerationInventoryScan(ctx, accelerationID, id)
}

func (s *Store) AppendClaimedAccelerationInventory(
	ctx context.Context,
	accelerationID, scanID, owner string,
	observedAt int64,
	objects []StorageInventoryObject,
	complete bool,
	now int64,
) error {
	scan, err := s.GetAccelerationInventoryScan(ctx, accelerationID, scanID)
	if err != nil {
		return err
	}
	if scan.State != "leased" || scan.Owner != strings.TrimSpace(owner) ||
		scan.LeaseExpiresAt <= now || (scan.ObservedAt != 0 && scan.ObservedAt != observedAt) {
		return ErrAccelerationInventoryScanInvalid
	}
	if observedAt <= 0 {
		return ErrAccelerationInventoryScanInvalid
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE acceleration_inventory_scans SET
		observed_at = ?, lease_expires_at = ?, updated_at = ?
		WHERE id = ? AND state = 'leased' AND owner = ?`, observedAt,
		now+(10*time.Minute).Milliseconds(), now, scanID, owner); err != nil {
		return err
	}
	if err := s.AppendAccelerationInventory(ctx, accelerationID, owner, scanID,
		observedAt, objects, complete, now); err != nil {
		return err
	}
	if !complete {
		return nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE acceleration_inventory_scans SET
		state = 'completed', owner = '', lease_expires_at = 0,
		completed_at = ?, updated_at = ?, last_error = ''
		WHERE id = ? AND state = 'leased'`, now, now, scanID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrAccelerationInventoryScanInvalid
	}
	return nil
}

func (s *Store) FailAccelerationInventoryScan(
	ctx context.Context,
	accelerationID, scanID, owner, message string,
	now int64,
) error {
	message = strings.TrimSpace(message)
	if len(message) > 2000 {
		message = message[:2000]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE acceleration_inventory_scans SET
		state = 'failed', owner = '', lease_expires_at = 0,
		last_error = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND acceleration_id = ? AND state = 'leased' AND owner = ?`,
		message, now, now, scanID, accelerationID, owner)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrAccelerationInventoryScanInvalid
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM acceleration_inventory_snapshots
		WHERE id = ?`, scanID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO acceleration_storage_status
		(acceleration_id, reconciliation_error) VALUES (?, ?)
		ON CONFLICT(acceleration_id) DO UPDATE SET reconciliation_error = excluded.reconciliation_error`,
		accelerationID, message); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetAccelerationInventoryScan(
	ctx context.Context,
	accelerationID, scanID string,
) (AccelerationInventoryScan, error) {
	var scan AccelerationInventoryScan
	err := s.db.QueryRowContext(ctx, `SELECT id, acceleration_id, owner, state,
		attempts, lease_expires_at, observed_at, last_error, requested_at,
		started_at, completed_at, updated_at FROM acceleration_inventory_scans
		WHERE acceleration_id = ? AND id = ?`, accelerationID, scanID).Scan(
		&scan.ID, &scan.AccelerationID, &scan.Owner, &scan.State, &scan.Attempts,
		&scan.LeaseExpiresAt, &scan.ObservedAt, &scan.LastError, &scan.RequestedAt,
		&scan.StartedAt, &scan.CompletedAt, &scan.UpdatedAt,
	)
	return scan, err
}

func (s *Store) LatestAccelerationInventoryScan(
	ctx context.Context,
	accelerationID string,
) (AccelerationInventoryScan, error) {
	var scan AccelerationInventoryScan
	err := s.db.QueryRowContext(ctx, `SELECT id, acceleration_id, owner, state,
		attempts, lease_expires_at, observed_at, last_error, requested_at,
		started_at, completed_at, updated_at FROM acceleration_inventory_scans
		WHERE acceleration_id = ? ORDER BY requested_at DESC, id DESC LIMIT 1`,
		accelerationID).Scan(&scan.ID, &scan.AccelerationID, &scan.Owner, &scan.State,
		&scan.Attempts, &scan.LeaseExpiresAt, &scan.ObservedAt, &scan.LastError,
		&scan.RequestedAt, &scan.StartedAt, &scan.CompletedAt, &scan.UpdatedAt)
	return scan, err
}

func (s *Store) activeAccelerationInventoryScan(
	ctx context.Context,
	accelerationID string,
) (AccelerationInventoryScan, error) {
	var scan AccelerationInventoryScan
	err := s.db.QueryRowContext(ctx, `SELECT id, acceleration_id, owner, state,
		attempts, lease_expires_at, observed_at, last_error, requested_at,
		started_at, completed_at, updated_at FROM acceleration_inventory_scans
		WHERE acceleration_id = ? AND state IN ('queued', 'leased') LIMIT 1`,
		accelerationID).Scan(&scan.ID, &scan.AccelerationID, &scan.Owner, &scan.State,
		&scan.Attempts, &scan.LeaseExpiresAt, &scan.ObservedAt, &scan.LastError,
		&scan.RequestedAt, &scan.StartedAt, &scan.CompletedAt, &scan.UpdatedAt)
	return scan, err
}

func (s *Store) ScheduleDueAccelerationInventoryScans(
	ctx context.Context,
	now int64,
) ([]AccelerationInventoryScan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT acceleration.id
		FROM accelerations acceleration
		LEFT JOIN acceleration_storage_status storage
		 ON storage.acceleration_id = acceleration.id
		WHERE acceleration.enabled = 1
		 AND NOT EXISTS (SELECT 1 FROM acceleration_inventory_scans active
		  WHERE active.acceleration_id = acceleration.id
		  AND active.state IN ('queued', 'leased'))
		 AND MAX(COALESCE(storage.last_reconciled_at, 0),
		  COALESCE((SELECT MAX(history.requested_at)
		   FROM acceleration_inventory_scans history
		   WHERE history.acceleration_id = acceleration.id), 0))
		  <= ? - acceleration.inventory_interval_seconds * 1000
		ORDER BY acceleration.id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	scans := make([]AccelerationInventoryScan, 0, len(ids))
	for _, id := range ids {
		scan, err := s.RequestAccelerationInventoryScan(ctx, id, now)
		if err != nil {
			return nil, err
		}
		scans = append(scans, scan)
	}
	return scans, nil
}
