package store

import (
	"context"
	"database/sql"
	"errors"
)

var (
	ErrBindingCodeInvalid            = errors.New("binding code is invalid")
	ErrBindingConflict               = errors.New("external subject is already linked")
	ErrBindingPrincipalUnavailable   = errors.New("binding principal is unavailable")
	ErrBindingIntegrationUnavailable = errors.New("integration credential is unavailable")
)

type ExternalBindingTarget struct {
	IntegrationID string
	AdapterID     string
	ScopeType     string
	ScopeID       string
	SubjectID     string
}

type ExternalBindingRedemption struct {
	Principal Principal
	Replayed  bool
}

// CreateExternalBindingCode replaces the Principal's previous code. Only an
// active OIDC Principal may own a binding code.
func (s *Store) CreateExternalBindingCode(
	ctx context.Context,
	principalID string,
	codeHash []byte,
	createdAt, expiresAt int64,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var eligible bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM users
			WHERE id = ? AND active = 1 AND COALESCE(oidc_subject, '') <> ''
		)`, principalID).Scan(&eligible); err != nil {
		return err
	}
	if !eligible {
		return ErrBindingPrincipalUnavailable
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM external_binding_codes WHERE principal_id = ?`, principalID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO external_binding_codes
			(code_hash, principal_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`,
		codeHash, principalID, createdAt, expiresAt); err != nil {
		return err
	}
	return tx.Commit()
}

// RedeemExternalBindingCode atomically consumes a code and links the external
// subject. An exact retry of a successful redemption is accepted until expiry;
// the same code can never target a different subject.
func (s *Store) RedeemExternalBindingCode(
	ctx context.Context,
	codeHash, integrationTokenHash []byte,
	target ExternalBindingTarget,
	now int64,
) (ExternalBindingRedemption, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalBindingRedemption{}, err
	}
	defer tx.Rollback()

	var integrationExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM integrations
			WHERE id = ? AND active = 1 AND token_hash = ?
		)`, target.IntegrationID, integrationTokenHash).Scan(&integrationExists); err != nil {
		return ExternalBindingRedemption{}, err
	}
	if !integrationExists {
		return ExternalBindingRedemption{}, ErrBindingIntegrationUnavailable
	}

	var principalID string
	var expiresAt int64
	var consumedAt sql.NullInt64
	var consumedIntegrationID, consumedAdapterID sql.NullString
	var consumedScopeType, consumedScopeID, consumedSubjectID sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT principal_id, expires_at, consumed_at,
		        consumed_integration_id, consumed_adapter_id,
		        consumed_scope_type, consumed_scope_id, consumed_subject_id
		 FROM external_binding_codes WHERE code_hash = ?`, codeHash).Scan(
		&principalID, &expiresAt, &consumedAt,
		&consumedIntegrationID, &consumedAdapterID,
		&consumedScopeType, &consumedScopeID, &consumedSubjectID,
	)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && now >= expiresAt) {
		return ExternalBindingRedemption{}, ErrBindingCodeInvalid
	}
	if err != nil {
		return ExternalBindingRedemption{}, err
	}

	if consumedAt.Valid {
		if consumedIntegrationID.String != target.IntegrationID ||
			consumedAdapterID.String != target.AdapterID ||
			consumedScopeType.String != target.ScopeType ||
			consumedScopeID.String != target.ScopeID ||
			consumedSubjectID.String != target.SubjectID {
			return ExternalBindingRedemption{}, ErrBindingCodeInvalid
		}
		principal, err := currentOIDCPrincipal(ctx, tx, principalID)
		if err != nil {
			return ExternalBindingRedemption{}, err
		}
		var linkedPrincipalID string
		err = tx.QueryRowContext(ctx,
			`SELECT principal_id FROM external_identity_links
			 WHERE integration_id = ? AND adapter_id = ? AND scope_type = ?
			   AND scope_id = ? AND subject_id = ?`,
			target.IntegrationID, target.AdapterID, target.ScopeType,
			target.ScopeID, target.SubjectID).Scan(&linkedPrincipalID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && linkedPrincipalID != principalID) {
			if err := tx.Commit(); err != nil {
				return ExternalBindingRedemption{}, err
			}
			return ExternalBindingRedemption{}, ErrBindingConflict
		}
		if err != nil {
			return ExternalBindingRedemption{}, err
		}
		if err := tx.Commit(); err != nil {
			return ExternalBindingRedemption{}, err
		}
		return ExternalBindingRedemption{Principal: principal, Replayed: true}, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE external_binding_codes
		 SET consumed_at = ?, consumed_integration_id = ?, consumed_adapter_id = ?,
		     consumed_scope_type = ?, consumed_scope_id = ?, consumed_subject_id = ?
		 WHERE code_hash = ? AND consumed_at IS NULL`,
		now, target.IntegrationID, target.AdapterID,
		target.ScopeType, target.ScopeID, target.SubjectID, codeHash); err != nil {
		return ExternalBindingRedemption{}, err
	}

	principal, principalErr := currentOIDCPrincipal(ctx, tx, principalID)
	if principalErr != nil {
		if errors.Is(principalErr, ErrBindingPrincipalUnavailable) {
			if err := tx.Commit(); err != nil {
				return ExternalBindingRedemption{}, err
			}
		}
		return ExternalBindingRedemption{}, principalErr
	}

	var linkedPrincipalID string
	err = tx.QueryRowContext(ctx,
		`SELECT principal_id FROM external_identity_links
		 WHERE integration_id = ? AND adapter_id = ? AND scope_type = ?
		   AND scope_id = ? AND subject_id = ?`,
		target.IntegrationID, target.AdapterID, target.ScopeType,
		target.ScopeID, target.SubjectID).Scan(&linkedPrincipalID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx,
			`INSERT INTO external_identity_links
				(integration_id, adapter_id, scope_type, scope_id, subject_id,
				 principal_id, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			target.IntegrationID, target.AdapterID, target.ScopeType,
			target.ScopeID, target.SubjectID, principalID, now, now)
		if err != nil {
			return ExternalBindingRedemption{}, err
		}
	case err != nil:
		return ExternalBindingRedemption{}, err
	case linkedPrincipalID != principalID:
		if err := tx.Commit(); err != nil {
			return ExternalBindingRedemption{}, err
		}
		return ExternalBindingRedemption{}, ErrBindingConflict
	}

	if err := tx.Commit(); err != nil {
		return ExternalBindingRedemption{}, err
	}
	return ExternalBindingRedemption{Principal: principal}, nil
}

func currentOIDCPrincipal(ctx context.Context, tx *sql.Tx, principalID string) (Principal, error) {
	principal, err := scanPrincipal(tx.QueryRowContext(ctx,
		`SELECT id, name, avatar, kind, COALESCE(oidc_subject, ''), roles_json,
		        active, created_at, updated_at
		 FROM users
		 WHERE id = ? AND active = 1 AND COALESCE(oidc_subject, '') <> ''`,
		principalID))
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrBindingPrincipalUnavailable
	}
	return principal, err
}

func (s *Store) PruneExternalBindingCodes(ctx context.Context, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM external_binding_codes WHERE expires_at <= ?`, now)
	return err
}
