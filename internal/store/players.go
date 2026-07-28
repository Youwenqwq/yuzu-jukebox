package store

import (
	"context"
	"database/sql"
)

type Player struct {
	ID            string
	Name          string
	Active        bool
	KeyConfigured bool
	CreatedAt     int64
	UpdatedAt     int64
	LastSeenAt    *int64
}

func (s *Store) CreatePlayer(ctx context.Context, id, name string, tokenHash []byte) (Player, error) {
	now := nowMs()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO players (id, name, token_hash, active, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		id, name, tokenHash, now, now,
	)
	if err != nil {
		return Player{}, err
	}
	return s.GetPlayer(ctx, id)
}

func (s *Store) GetPlayer(ctx context.Context, id string) (Player, error) {
	return scanPlayer(s.db.QueryRowContext(ctx,
		`SELECT id, name, active, token_hash IS NOT NULL, created_at, updated_at, last_seen_at
		 FROM players WHERE id = ?`, id))
}

func (s *Store) ListPlayers(ctx context.Context) ([]Player, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, active, token_hash IS NOT NULL, created_at, updated_at, last_seen_at
		 FROM players ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	players := make([]Player, 0)
	for rows.Next() {
		player, err := scanPlayer(rows)
		if err != nil {
			return nil, err
		}
		players = append(players, player)
	}
	return players, rows.Err()
}

func (s *Store) UpdatePlayer(ctx context.Context, id, name string, active bool) (Player, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE players SET name = ?, active = ?, updated_at = ? WHERE id = ?`,
		name, active, nowMs(), id,
	)
	if err != nil {
		return Player{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Player{}, err
	} else if affected == 0 {
		return Player{}, sql.ErrNoRows
	}
	return s.GetPlayer(ctx, id)
}

func (s *Store) UpdatePlayerToken(ctx context.Context, id string, tokenHash []byte) (Player, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE players SET token_hash = ?, updated_at = ? WHERE id = ?`,
		tokenHash, nowMs(), id,
	)
	if err != nil {
		return Player{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Player{}, err
	} else if affected == 0 {
		return Player{}, sql.ErrNoRows
	}
	return s.GetPlayer(ctx, id)
}

func (s *Store) DeletePlayer(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM players WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ResolvePlayerToken(ctx context.Context, tokenHash []byte) (Player, error) {
	return scanPlayer(s.db.QueryRowContext(ctx,
		`SELECT id, name, active, token_hash IS NOT NULL, created_at, updated_at, last_seen_at
		 FROM players WHERE token_hash = ? AND active = 1`, tokenHash))
}

func (s *Store) TouchPlayer(ctx context.Context, id string, seenAt int64) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE players SET last_seen_at = ? WHERE id = ? AND active = 1`, seenAt, id)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type playerScanner interface {
	Scan(dest ...any) error
}

func scanPlayer(row playerScanner) (Player, error) {
	var player Player
	var lastSeen sql.NullInt64
	err := row.Scan(
		&player.ID, &player.Name, &player.Active, &player.KeyConfigured,
		&player.CreatedAt, &player.UpdatedAt, &lastSeen,
	)
	if lastSeen.Valid {
		player.LastSeenAt = &lastSeen.Int64
	}
	return player, err
}
