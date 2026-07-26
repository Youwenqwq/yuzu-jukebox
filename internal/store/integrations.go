package store

import (
	"context"
	"database/sql"
)

// Integration is a trusted external client. TokenHash is never exposed by HTTP APIs.
type Integration struct {
	ID         string
	Name       string
	TokenHash  []byte
	Active     bool
	CreatedAt  int64
	UpdatedAt  int64
	LastUsedAt *int64
}

func (s *Store) CreateIntegration(ctx context.Context, id, name string, tokenHash []byte) (Integration, error) {
	now := nowMs()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO integrations (id, name, token_hash, active, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`, id, name, tokenHash, now, now)
	if err != nil {
		return Integration{}, err
	}
	return s.GetIntegration(ctx, id)
}

func (s *Store) GetIntegration(ctx context.Context, id string) (Integration, error) {
	return scanIntegration(s.db.QueryRowContext(ctx,
		`SELECT id, name, token_hash, active, created_at, updated_at, last_used_at
		 FROM integrations WHERE id = ?`, id))
}

func (s *Store) ResolveIntegrationToken(ctx context.Context, tokenHash []byte) (Integration, error) {
	return scanIntegration(s.db.QueryRowContext(ctx,
		`SELECT id, name, token_hash, active, created_at, updated_at, last_used_at
		 FROM integrations WHERE token_hash = ? AND active = 1`, tokenHash))
}

func (s *Store) ListIntegrations(ctx context.Context) ([]Integration, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, token_hash, active, created_at, updated_at, last_used_at
		 FROM integrations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Integration, 0)
	for rows.Next() {
		integration, err := scanIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, integration)
	}
	return out, rows.Err()
}

func (s *Store) UpdateIntegration(ctx context.Context, id, name string, active bool) (Integration, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE integrations SET name = ?, active = ?, updated_at = ? WHERE id = ?`,
		name, active, nowMs(), id)
	if err != nil {
		return Integration{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Integration{}, err
	} else if affected == 0 {
		return Integration{}, sql.ErrNoRows
	}
	return s.GetIntegration(ctx, id)
}

func (s *Store) RotateIntegrationToken(ctx context.Context, id string, tokenHash []byte) (Integration, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE integrations SET token_hash = ?, updated_at = ? WHERE id = ?`, tokenHash, nowMs(), id)
	if err != nil {
		return Integration{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Integration{}, err
	} else if affected == 0 {
		return Integration{}, sql.ErrNoRows
	}
	return s.GetIntegration(ctx, id)
}

func (s *Store) TouchIntegration(ctx context.Context, id string, usedAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE integrations SET last_used_at = ? WHERE id = ?`, usedAt, id)
	return err
}

// DeleteIntegration removes the credential and its Integration-owned mappings.
// Principals and Room grants remain because they are shared Yuzu resources.
func (s *Store) DeleteIntegration(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM sessions WHERE integration_id = ?`,
		`DELETE FROM idempotency_records WHERE integration_id = ?`,
		`DELETE FROM external_identity_links WHERE integration_id = ?`,
		`DELETE FROM external_scope_rooms WHERE integration_id = ?`,
		`DELETE FROM integrations WHERE id = ?`,
	} {
		result, err := tx.ExecContext(ctx, statement, id)
		if err != nil {
			return err
		}
		if statement == `DELETE FROM integrations WHERE id = ?` {
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				return sql.ErrNoRows
			}
		}
	}
	return tx.Commit()
}

type integrationScanner interface {
	Scan(dest ...any) error
}

func scanIntegration(row integrationScanner) (Integration, error) {
	var integration Integration
	var lastUsed sql.NullInt64
	err := row.Scan(
		&integration.ID,
		&integration.Name,
		&integration.TokenHash,
		&integration.Active,
		&integration.CreatedAt,
		&integration.UpdatedAt,
		&lastUsed,
	)
	if err != nil {
		return Integration{}, err
	}
	if lastUsed.Valid {
		integration.LastUsedAt = &lastUsed.Int64
	}
	return integration, nil
}
