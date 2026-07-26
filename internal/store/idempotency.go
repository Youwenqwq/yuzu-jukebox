package store

import (
	"context"
	"database/sql"
)

type IdempotencyRecord struct {
	ActorID       string
	IntegrationID string
	Key           string
	Method        string
	Path          string
	RequestHash   []byte
	StatusCode    *int
	ResponseBody  []byte
	ExpiresAt     int64
}

// BeginIdempotency atomically reserves a key. created=false returns the current
// completed or in-progress record for the same actor and operation.
func (s *Store) BeginIdempotency(
	ctx context.Context,
	record IdempotencyRecord,
) (current IdempotencyRecord, created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_records WHERE expires_at <= ?`, nowMs()); err != nil {
		return IdempotencyRecord{}, false, err
	}
	result, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO idempotency_records
		 (actor_id, integration_id, key, method, path, request_hash, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ActorID, record.IntegrationID, record.Key, record.Method,
		record.Path, record.RequestHash, record.ExpiresAt, nowMs())
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	if affected == 1 {
		if err := tx.Commit(); err != nil {
			return IdempotencyRecord{}, false, err
		}
		return record, true, nil
	}
	current, err = scanIdempotency(tx.QueryRowContext(ctx,
		`SELECT actor_id, integration_id, key, method, path, request_hash,
		        status_code, response_body, expires_at
		 FROM idempotency_records
		 WHERE actor_id = ? AND integration_id = ? AND key = ? AND method = ? AND path = ?`,
		record.ActorID, record.IntegrationID, record.Key, record.Method, record.Path))
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return IdempotencyRecord{}, false, err
	}
	return current, false, nil
}

func (s *Store) CompleteIdempotency(
	ctx context.Context,
	record IdempotencyRecord,
) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE idempotency_records
		 SET status_code = ?, response_body = ?, expires_at = ?
		 WHERE actor_id = ? AND integration_id = ? AND key = ? AND method = ? AND path = ?`,
		valueOrNil(record.StatusCode), record.ResponseBody, record.ExpiresAt,
		record.ActorID, record.IntegrationID, record.Key, record.Method, record.Path)
	return err
}

func (s *Store) DeleteIdempotency(
	ctx context.Context,
	actorID, integrationID, key, method, path string,
) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_records
		 WHERE actor_id = ? AND integration_id = ? AND key = ? AND method = ? AND path = ?`,
		actorID, integrationID, key, method, path)
	return err
}

func (s *Store) PruneIdempotency(ctx context.Context, now int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_records WHERE expires_at <= ?`, now)
	return err
}

type idempotencyScanner interface {
	Scan(dest ...any) error
}

func scanIdempotency(row idempotencyScanner) (IdempotencyRecord, error) {
	var record IdempotencyRecord
	var status sql.NullInt64
	err := row.Scan(
		&record.ActorID,
		&record.IntegrationID,
		&record.Key,
		&record.Method,
		&record.Path,
		&record.RequestHash,
		&status,
		&record.ResponseBody,
		&record.ExpiresAt,
	)
	if err != nil {
		return IdempotencyRecord{}, err
	}
	if status.Valid {
		value := int(status.Int64)
		record.StatusCode = &value
	}
	return record, nil
}

func valueOrNil(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
