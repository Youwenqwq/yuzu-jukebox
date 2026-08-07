package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/pressly/goose/v3"
)

func init() {
	goose.AddMigrationContext(upHashSessionTokens, downHashSessionTokens)
}

func upHashSessionTokens(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT token FROM sessions`)
	if err != nil {
		return fmt.Errorf("select session tokens: %w", err)
	}
	var tokens []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan session token: %w", err)
		}
		tokens = append(tokens, token)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close session token rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate session tokens: %w", err)
	}

	for _, token := range tokens {
		digest := sha256.Sum256([]byte(token))
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET token = ? WHERE token = ?`,
			hex.EncodeToString(digest[:]), token,
		); err != nil {
			return fmt.Errorf("hash session token: %w", err)
		}
	}
	return nil
}

func downHashSessionTokens(context.Context, *sql.Tx) error {
	return errors.New("session token hashes cannot be reversed")
}
