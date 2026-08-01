package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrAccelerationStorageFull      = errors.New("acceleration storage budget exceeded")
	ErrAccelerationStorageUnmanaged = errors.New("acceleration storage budget is not configured")
	ErrStorageReservationInvalid    = errors.New("storage reservation invalid")
	ErrStorageReservationInProgress = errors.New("storage locator is already reserved")
	ErrStorageDeletionInvalid       = errors.New("storage deletion invalid")
	ErrStorageInventoryInvalid      = errors.New("storage inventory invalid")
)

type AccelerationObject struct {
	AccelerationID  string `json:"acceleration_id"`
	Locator         string `json:"locator"`
	ContentVersion  string `json:"content_version"`
	SizeBytes       int64  `json:"size_bytes"`
	ExternalVersion string `json:"external_version,omitempty"`
	State           string `json:"state"`
	ReferenceCount  int64  `json:"reference_count"`
	LastAccessedAt  int64  `json:"last_accessed_at"`
	LastObservedAt  int64  `json:"last_observed_at,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

type StorageReservation struct {
	LeaseID        string `json:"lease_id"`
	AccelerationID string `json:"acceleration_id"`
	Locator        string `json:"locator"`
	SizeBytes      int64  `json:"size_bytes"`
	ExpiresAt      int64  `json:"expires_at"`
	AlreadyPresent bool   `json:"already_present"`
}

type StorageInventoryObject struct {
	Locator         string `json:"locator"`
	SizeBytes       int64  `json:"size_bytes"`
	ExternalVersion string `json:"external_version,omitempty"`
}

type StorageDeletion struct {
	ID             string `json:"id"`
	AccelerationID string `json:"acceleration_id"`
	Locator        string `json:"locator"`
	Owner          string `json:"owner"`
	Attempts       int64  `json:"attempts"`
	ExpiresAt      int64  `json:"expires_at"`
}

type AccelerationStorageStatus struct {
	Managed              bool   `json:"managed"`
	BudgetBytes          int64  `json:"budget_bytes"`
	HighWatermarkPercent int    `json:"high_watermark_percent"`
	LowWatermarkPercent  int    `json:"low_watermark_percent"`
	AccountedBytes       int64  `json:"accounted_bytes"`
	ReservedBytes        int64  `json:"reserved_bytes"`
	ObservedBytes        int64  `json:"observed_bytes"`
	ManagedObjectCount   int64  `json:"managed_object_count"`
	ObservedObjectCount  int64  `json:"observed_object_count"`
	OrphanCount          int64  `json:"orphan_count"`
	MissingCount         int64  `json:"missing_count"`
	PendingDeletionCount int64  `json:"pending_deletion_count"`
	ObservedAt           int64  `json:"observed_at,omitempty"`
	StaleAfterSeconds    int    `json:"stale_after_seconds"`
	Stale                bool   `json:"stale"`
	ReconciliationError  string `json:"reconciliation_error,omitempty"`
	Pressure             string `json:"pressure"`
}

func (s *Store) ReserveAccelerationStorage(
	ctx context.Context,
	leaseID, owner, locator string,
	sizeBytes, now int64,
) (StorageReservation, error) {
	locator = strings.TrimSpace(locator)
	if locator == "" || len(locator) > 2048 || sizeBytes <= 0 {
		return StorageReservation{}, ErrStorageReservationInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StorageReservation{}, err
	}
	defer tx.Rollback()
	lease, _, err := distributionLeaseForUpdate(ctx, tx, leaseID, owner, now)
	if err != nil {
		return StorageReservation{}, err
	}
	var budget int64
	var high, low int
	if err := tx.QueryRowContext(ctx, `SELECT storage_budget_bytes,
		storage_high_watermark_percent, storage_low_watermark_percent
		FROM accelerations WHERE id = ?`, lease.AccelerationID).Scan(&budget, &high, &low); err != nil {
		return StorageReservation{}, err
	}
	if budget <= 0 {
		return StorageReservation{}, ErrAccelerationStorageUnmanaged
	}
	reservation := StorageReservation{LeaseID: lease.ID, AccelerationID: lease.AccelerationID,
		Locator: locator, SizeBytes: sizeBytes, ExpiresAt: lease.ExpiresAt}
	var existingSize int64
	var existingState string
	err = tx.QueryRowContext(ctx, `SELECT size_bytes, state FROM acceleration_objects
		WHERE acceleration_id = ? AND locator = ?`,
		lease.AccelerationID, locator).Scan(&existingSize, &existingState)
	if err == nil {
		switch existingState {
		case "ready", "orphan":
			if existingSize != sizeBytes {
				return StorageReservation{}, fmt.Errorf("%w: existing object size mismatch", ErrStorageReservationInvalid)
			}
			reservation.AlreadyPresent = true
			if err := tx.Commit(); err != nil {
				return StorageReservation{}, err
			}
			return reservation, nil
		case "deleting":
			return StorageReservation{}, ErrStorageReservationInProgress
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return StorageReservation{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM acceleration_storage_reservations
		WHERE acceleration_id = ? AND expires_at <= ?`, lease.AccelerationID, now); err != nil {
		return StorageReservation{}, err
	}
	var competingReservation int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM acceleration_storage_reservations
		WHERE acceleration_id = ? AND locator = ? AND lease_id <> ? AND expires_at > ?`,
		lease.AccelerationID, locator, lease.ID, now).Scan(&competingReservation)
	if err == nil {
		return StorageReservation{}, ErrStorageReservationInProgress
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return StorageReservation{}, err
	}
	var accounted, reserved int64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT SUM(size_bytes) FROM acceleration_objects
		 WHERE acceleration_id = ? AND state <> 'missing'), 0),
		COALESCE((SELECT SUM(size_bytes) FROM acceleration_storage_reservations
		 WHERE acceleration_id = ? AND expires_at > ? AND lease_id <> ?), 0)`,
		lease.AccelerationID, lease.AccelerationID, now, lease.ID).Scan(&accounted, &reserved); err != nil {
		return StorageReservation{}, err
	}
	highBytes := budget * int64(high) / 100
	if accounted+reserved+sizeBytes > highBytes {
		if err := enqueueStorageGCTx(ctx, tx, lease.AccelerationID,
			budget*int64(low)/100-sizeBytes, locator, now); err != nil {
			return StorageReservation{}, err
		}
		if err := tx.Commit(); err != nil {
			return StorageReservation{}, err
		}
		return StorageReservation{}, ErrAccelerationStorageFull
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO acceleration_storage_reservations
		(lease_id, acceleration_id, locator, size_bytes, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(lease_id) DO UPDATE SET locator = excluded.locator,
		size_bytes = excluded.size_bytes, expires_at = excluded.expires_at`,
		lease.ID, lease.AccelerationID, locator, sizeBytes, lease.ExpiresAt, now)
	if err != nil {
		return StorageReservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return StorageReservation{}, err
	}
	return reservation, nil
}

func (s *Store) AccelerationStorageStatus(
	ctx context.Context,
	accelerationID string,
	now int64,
) (AccelerationStorageStatus, error) {
	var status AccelerationStorageStatus
	if err := s.db.QueryRowContext(ctx, `SELECT storage_budget_bytes,
		storage_high_watermark_percent, storage_low_watermark_percent,
		inventory_stale_after_seconds FROM accelerations WHERE id = ?`, accelerationID).Scan(
		&status.BudgetBytes, &status.HighWatermarkPercent, &status.LowWatermarkPercent,
		&status.StaleAfterSeconds,
	); err != nil {
		return AccelerationStorageStatus{}, err
	}
	status.Managed = status.BudgetBytes > 0
	if err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN state <> 'missing' THEN size_bytes ELSE 0 END), 0),
		COUNT(*), COALESCE(SUM(CASE WHEN state = 'orphan' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN state = 'missing' THEN 1 ELSE 0 END), 0)
		FROM acceleration_objects WHERE acceleration_id = ?`, accelerationID).Scan(
		&status.AccountedBytes, &status.ManagedObjectCount, &status.OrphanCount, &status.MissingCount,
	); err != nil {
		return AccelerationStorageStatus{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0)
		FROM acceleration_storage_reservations WHERE acceleration_id = ? AND expires_at > ?`,
		accelerationID, now).Scan(&status.ReservedBytes); err != nil {
		return AccelerationStorageStatus{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM acceleration_deletion_jobs
		WHERE acceleration_id = ?`, accelerationID).Scan(&status.PendingDeletionCount); err != nil {
		return AccelerationStorageStatus{}, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT observed_bytes, observed_object_count,
		last_reconciled_at, reconciliation_error FROM acceleration_storage_status
		WHERE acceleration_id = ?`, accelerationID).Scan(&status.ObservedBytes,
		&status.ObservedObjectCount, &status.ObservedAt, &status.ReconciliationError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AccelerationStorageStatus{}, err
	}
	status.Stale = status.ObservedAt == 0 ||
		now-status.ObservedAt > int64(status.StaleAfterSeconds)*1000
	status.Pressure = storagePressure(status)
	return status, nil
}

func storagePressure(status AccelerationStorageStatus) string {
	if !status.Managed {
		return "unmanaged"
	}
	used := status.AccountedBytes + status.ReservedBytes
	if used >= status.BudgetBytes {
		return "full"
	}
	if used >= status.BudgetBytes*int64(status.HighWatermarkPercent)/100 {
		return "high"
	}
	return "normal"
}

func (s *Store) AppendAccelerationInventory(
	ctx context.Context,
	accelerationID, owner, snapshotID string,
	observedAt int64,
	objects []StorageInventoryObject,
	complete bool,
	now int64,
) error {
	owner = strings.TrimSpace(owner)
	snapshotID = strings.TrimSpace(snapshotID)
	if owner == "" || len(owner) > 200 || snapshotID == "" || len(snapshotID) > 200 || observedAt <= 0 {
		return ErrStorageInventoryInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO acceleration_inventory_snapshots
		(id, acceleration_id, owner, observed_at, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`, snapshotID, accelerationID, owner, observedAt, now)
	if err != nil {
		return err
	}
	var storedAcceleration, storedOwner string
	if err := tx.QueryRowContext(ctx, `SELECT acceleration_id, owner
		FROM acceleration_inventory_snapshots WHERE id = ?`, snapshotID).Scan(
		&storedAcceleration, &storedOwner); err != nil {
		return err
	}
	if storedAcceleration != accelerationID || storedOwner != owner {
		return ErrStorageInventoryInvalid
	}
	for _, object := range objects {
		object.Locator = strings.TrimSpace(object.Locator)
		if len(object.Locator) > 2048 || len(object.ExternalVersion) > 1024 {
			return ErrStorageInventoryInvalid
		}
		if object.Locator == "" || object.SizeBytes < 0 {
			return ErrStorageInventoryInvalid
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO acceleration_inventory_objects
			(snapshot_id, acceleration_id, locator, size_bytes, external_version)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(snapshot_id, locator) DO UPDATE SET
			size_bytes = excluded.size_bytes, external_version = excluded.external_version`,
			snapshotID, accelerationID, object.Locator, object.SizeBytes,
			strings.TrimSpace(object.ExternalVersion)); err != nil {
			return err
		}
	}
	if !complete {
		return tx.Commit()
	}
	// 快照描述的是存储在 observedAt 时刻的样子，分页扫描本身要花好几秒。
	// 扫描开始之后才落库的对象不可能出现在快照里，不由本次扫描判决，
	// 否则并发上传会被误标 missing：既不计入 accounted_bytes 也进不了 GC 受害者集合。
	if _, err := tx.ExecContext(ctx, `UPDATE acceleration_objects SET state = 'missing',
		last_observed_at = ?, updated_at = ? WHERE acceleration_id = ?
		AND state NOT IN ('deleting', 'delete_failed') AND created_at < ? AND locator NOT IN
		(SELECT locator FROM acceleration_inventory_objects WHERE snapshot_id = ?)`,
		observedAt, now, accelerationID, observedAt, snapshotID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO acceleration_objects
		(acceleration_id, locator, content_version, size_bytes, external_version,
		 state, reference_count, last_accessed_at, last_observed_at, created_at, updated_at)
		SELECT ?, inventory.locator, '', inventory.size_bytes, inventory.external_version,
		 CASE WHEN EXISTS (SELECT 1 FROM distribution_candidates candidate
		  WHERE candidate.acceleration_id = ? AND candidate.locator = inventory.locator)
		  THEN 'ready' ELSE 'orphan' END,
		 (SELECT COUNT(*) FROM distribution_candidates candidate
		  WHERE candidate.acceleration_id = ? AND candidate.locator = inventory.locator),
		 ?, ?, ?, ? FROM acceleration_inventory_objects inventory
		 WHERE inventory.snapshot_id = ?
		ON CONFLICT(acceleration_id, locator) DO UPDATE SET
		 size_bytes = excluded.size_bytes, external_version = excluded.external_version,
		 state = CASE WHEN acceleration_objects.state IN ('deleting', 'delete_failed')
		  THEN acceleration_objects.state ELSE excluded.state END,
		 reference_count = excluded.reference_count,
		 last_observed_at = excluded.last_observed_at, updated_at = excluded.updated_at`,
		accelerationID, accelerationID, accelerationID, observedAt, observedAt, now, now,
		snapshotID); err != nil {
		return err
	}
	var observedBytes, objectCount, orphanCount, missingCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0), COUNT(*)
		FROM acceleration_inventory_objects WHERE snapshot_id = ?`, snapshotID).Scan(
		&observedBytes, &objectCount); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN state = 'orphan' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN state = 'missing' THEN 1 ELSE 0 END), 0)
		FROM acceleration_objects WHERE acceleration_id = ?`, accelerationID).Scan(
		&orphanCount, &missingCount); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO acceleration_storage_status
		(acceleration_id, observed_bytes, observed_object_count, orphan_count,
		 missing_count, last_reconciled_at, reconciliation_error)
		VALUES (?, ?, ?, ?, ?, ?, '') ON CONFLICT(acceleration_id) DO UPDATE SET
		 observed_bytes = excluded.observed_bytes,
		 observed_object_count = excluded.observed_object_count,
		 orphan_count = excluded.orphan_count, missing_count = excluded.missing_count,
		 last_reconciled_at = excluded.last_reconciled_at, reconciliation_error = ''`,
		accelerationID, observedBytes, objectCount, orphanCount, missingCount, observedAt)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM acceleration_inventory_snapshots
		WHERE id = ?`, snapshotID); err != nil {
		return err
	}
	var budget int64
	var high, low int
	if err := tx.QueryRowContext(ctx, `SELECT storage_budget_bytes,
		storage_high_watermark_percent, storage_low_watermark_percent
		FROM accelerations WHERE id = ?`, accelerationID).Scan(&budget, &high, &low); err != nil {
		return err
	}
	if budget > 0 && observedBytes > budget*int64(high)/100 {
		if err := enqueueStorageGCTx(ctx, tx, accelerationID,
			budget*int64(low)/100, "", now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ClaimAccelerationDeletion(
	ctx context.Context,
	accelerationID, owner string,
	ttl time.Duration,
	now int64,
) (StorageDeletion, error) {
	if strings.TrimSpace(owner) == "" {
		return StorageDeletion{}, ErrStorageDeletionInvalid
	}
	if ttl <= 0 || ttl > 10*time.Minute {
		ttl = 10 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StorageDeletion{}, err
	}
	defer tx.Rollback()
	var deletion StorageDeletion
	err = tx.QueryRowContext(ctx, `SELECT id, locator, attempts FROM acceleration_deletion_jobs
		WHERE acceleration_id = ? AND (state = 'pending'
		 OR (state IN ('failed', 'leased') AND lease_expires_at <= ?))
		ORDER BY updated_at, id LIMIT 1`, accelerationID, now).Scan(
		&deletion.ID, &deletion.Locator, &deletion.Attempts)
	if err != nil {
		return StorageDeletion{}, err
	}
	deletion.AccelerationID = accelerationID
	deletion.Owner = owner
	deletion.Attempts++
	deletion.ExpiresAt = now + ttl.Milliseconds()
	result, err := tx.ExecContext(ctx, `UPDATE acceleration_deletion_jobs SET
		owner = ?, state = 'leased', attempts = ?, lease_expires_at = ?,
		last_error = '', updated_at = ? WHERE id = ?`, owner, deletion.Attempts,
		deletion.ExpiresAt, now, deletion.ID)
	if err != nil {
		return StorageDeletion{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return StorageDeletion{}, ErrStorageDeletionInvalid
	}
	if err := tx.Commit(); err != nil {
		return StorageDeletion{}, err
	}
	return deletion, nil
}

func (s *Store) CompleteAccelerationDeletion(
	ctx context.Context,
	accelerationID, deletionID, owner string,
	now int64,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var locator string
	err = tx.QueryRowContext(ctx, `SELECT locator FROM acceleration_deletion_jobs
		WHERE id = ? AND acceleration_id = ? AND owner = ? AND state = 'leased'
		AND lease_expires_at > ?`, deletionID, accelerationID, owner, now).Scan(&locator)
	if err != nil {
		return ErrStorageDeletionInvalid
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM acceleration_objects
		WHERE acceleration_id = ? AND locator = ?`, accelerationID, locator); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM acceleration_deletion_jobs WHERE id = ?`, deletionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailAccelerationDeletion(
	ctx context.Context,
	accelerationID, deletionID, owner, message string,
	retryAt, now int64,
) error {
	result, err := s.db.ExecContext(ctx, `UPDATE acceleration_deletion_jobs SET
		state = 'failed', owner = '', lease_expires_at = ?, last_error = ?, updated_at = ?
		WHERE id = ? AND acceleration_id = ? AND owner = ? AND state = 'leased'`,
		retryAt, strings.TrimSpace(message), now, deletionID, accelerationID, owner)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrStorageDeletionInvalid
	}
	return nil
}

func (s *Store) TouchAccelerationObject(
	ctx context.Context,
	accelerationID, trackRef string,
	accessedAt int64,
) error {
	_, err := s.db.ExecContext(ctx, `UPDATE acceleration_objects SET last_accessed_at = ?,
		updated_at = ? WHERE acceleration_id = ? AND locator =
		(SELECT locator FROM distribution_candidates WHERE acceleration_id = ? AND track_ref = ?)`,
		accessedAt, accessedAt, accelerationID, accelerationID, trackRef)
	return err
}

// PinAccelerationDemand 把预取视界写成钉住集合。trackRefs 必须按紧迫度升序传入。
// 请求侧记紧迫度（影响认领顺序），对象侧记不可驱逐（GC 跳过）。
//
// prefetch_and_heat 模式下对象钉住按紧迫度累计体积，越过 prefetch_share_percent 后
// 停止钉住——待播优先级高于热度常驻，但不能扫穿它：一次点满几十首的队列如果全部
// 钉住，整个热集会被冲光，然后热集又要重传一遍，抖动只是换了个方向回来。越过上限
// 的部分退化成普通请求，最坏情况是那一首走源站 fallback。
// prefetch 模式下没有热集需要保护，待播可以用满预算。
//
// 钉住不做显式清理：它是 deadline 形状的，离开视界的曲目等钉住到期后自然落回热度池，
// 而不是一离开就被回收——刚放完的曲目按定义是缓存里最热的东西。
func (s *Store) PinAccelerationDemand(
	ctx context.Context,
	accelerationID string,
	trackRefs []string,
	pinnedUntil, now int64,
) error {
	if len(trackRefs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var budget int64
	var sharePercent int
	var cacheMode string
	if err := tx.QueryRowContext(ctx, `SELECT storage_budget_bytes,
		prefetch_share_percent, cache_mode FROM accelerations WHERE id = ?`,
		accelerationID).Scan(&budget, &sharePercent, &cacheMode); err != nil {
		return err
	}
	shareLimit := int64(-1)
	if budget > 0 && cacheMode == "prefetch_and_heat" {
		shareLimit = budget * int64(sharePercent) / 100
	}
	var pinnedBytes int64
	for _, ref := range trackRefs {
		if _, err := tx.ExecContext(ctx, `UPDATE distribution_requests SET pinned_until = ?
			WHERE acceleration_id = ? AND track_ref = ?`,
			pinnedUntil, accelerationID, ref); err != nil {
			return err
		}
		var locator string
		var size int64
		err := tx.QueryRowContext(ctx, `SELECT object.locator, object.size_bytes
			FROM distribution_candidates AS candidate
			JOIN acceleration_objects AS object
			 ON object.acceleration_id = candidate.acceleration_id
			 AND object.locator = candidate.locator
			WHERE candidate.acceleration_id = ? AND candidate.track_ref = ?`,
			accelerationID, ref).Scan(&locator, &size)
		if errors.Is(err, sql.ErrNoRows) {
			// 还没发布：只有请求侧的紧迫度，没有对象可钉。
			continue
		}
		if err != nil {
			return err
		}
		if shareLimit >= 0 && pinnedBytes+size > shareLimit {
			continue
		}
		pinnedBytes += size
		if _, err := tx.ExecContext(ctx, `UPDATE acceleration_objects
			SET pinned_until = ?, updated_at = ? WHERE acceleration_id = ? AND locator = ?`,
			pinnedUntil, now, accelerationID, locator); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func enqueueStorageGCTx(
	ctx context.Context,
	tx *sql.Tx,
	accelerationID string,
	targetBytes int64,
	excludeLocator string,
	now int64,
) error {
	var used int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0)
		FROM acceleration_objects WHERE acceleration_id = ? AND state <> 'missing'`,
		accelerationID).Scan(&used); err != nil {
		return err
	}
	if targetBytes < 0 {
		targetBytes = 0
	}
	// 钉住的对象不进受害者集合：正在播放或马上要放的曲目不能被回收。凑不够目标
	// 就凑不够——调用方会拿到 507，这比删掉马上要放的那一首正确。
	rows, err := tx.QueryContext(ctx, `SELECT locator, size_bytes FROM acceleration_objects
		WHERE acceleration_id = ? AND state IN ('ready', 'orphan', 'delete_failed')
		AND locator <> ? AND pinned_until <= ? ORDER BY CASE WHEN state = 'orphan' THEN 0 ELSE 1 END,
		last_accessed_at, locator`, accelerationID, excludeLocator, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	type victim struct {
		locator string
		size    int64
	}
	victims := make([]victim, 0)
	for rows.Next() && used > targetBytes {
		var item victim
		if err := rows.Scan(&item.locator, &item.size); err != nil {
			return err
		}
		victims = append(victims, item)
		used -= item.size
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range victims {
		// 先把驱逐写回需求侧，再删 candidate：关联要经过 candidate，顺序反了就丢了。
		// 被驱逐的请求退出可认领集合，只有真实需求（播放或缓存就绪）才会重新唤醒它。
		if _, err := tx.ExecContext(ctx, `UPDATE distribution_requests SET
			evicted_at = ?, updated_at = ? WHERE acceleration_id = ? AND track_ref IN
			(SELECT track_ref FROM distribution_candidates
			 WHERE acceleration_id = ? AND locator = ?)`,
			now, now, accelerationID, accelerationID, item.locator); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM distribution_candidates
			WHERE acceleration_id = ? AND locator = ?`, accelerationID, item.locator); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE acceleration_objects SET
			state = 'deleting', reference_count = 0, updated_at = ?
			WHERE acceleration_id = ? AND locator = ?`, now, accelerationID, item.locator); err != nil {
			return err
		}
		deletionID, err := randomStorageID("del_")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO acceleration_deletion_jobs
			(id, acceleration_id, locator, state, created_at, updated_at)
			VALUES (?, ?, ?, 'pending', ?, ?)
			ON CONFLICT(acceleration_id, locator) DO UPDATE SET
			state = 'pending', owner = '', lease_expires_at = 0, updated_at = excluded.updated_at`,
			deletionID, accelerationID, item.locator, now, now); err != nil {
			return err
		}
	}
	return nil
}

func randomStorageID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}
