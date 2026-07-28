package store

import (
	"context"
	"database/sql"
)

type RoomPlayerBinding struct {
	RoomID    string
	PlayerID  string
	CreatedAt int64
	UpdatedAt int64
}

func (s *Store) BindRoomPlayer(ctx context.Context, roomID, playerID string) (RoomPlayerBinding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RoomPlayerBinding{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM room_player_bindings WHERE player_id = ? AND room_id <> ?`,
		playerID, roomID); err != nil {
		return RoomPlayerBinding{}, err
	}
	now := nowMs()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO room_player_bindings (room_id, player_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(room_id) DO UPDATE SET
			player_id = excluded.player_id,
			updated_at = excluded.updated_at`,
		roomID, playerID, now, now); err != nil {
		return RoomPlayerBinding{}, err
	}
	binding, err := scanRoomPlayerBinding(tx.QueryRowContext(ctx,
		`SELECT room_id, player_id, created_at, updated_at
		 FROM room_player_bindings WHERE room_id = ?`, roomID))
	if err != nil {
		return RoomPlayerBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return RoomPlayerBinding{}, err
	}
	return binding, nil
}

func (s *Store) GetRoomPlayerBinding(ctx context.Context, roomID string) (RoomPlayerBinding, error) {
	return scanRoomPlayerBinding(s.db.QueryRowContext(ctx,
		`SELECT room_id, player_id, created_at, updated_at
		 FROM room_player_bindings WHERE room_id = ?`, roomID))
}

func (s *Store) GetRoomPlayerBindingByPlayer(ctx context.Context, playerID string) (RoomPlayerBinding, error) {
	return scanRoomPlayerBinding(s.db.QueryRowContext(ctx,
		`SELECT room_id, player_id, created_at, updated_at
		 FROM room_player_bindings WHERE player_id = ?`, playerID))
}

func (s *Store) UnbindRoomPlayer(ctx context.Context, roomID, playerID string) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM room_player_bindings WHERE room_id = ? AND player_id = ?`,
		roomID, playerID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type roomPlayerBindingScanner interface {
	Scan(dest ...any) error
}

func scanRoomPlayerBinding(row roomPlayerBindingScanner) (RoomPlayerBinding, error) {
	var binding RoomPlayerBinding
	err := row.Scan(&binding.RoomID, &binding.PlayerID, &binding.CreatedAt, &binding.UpdatedAt)
	return binding, err
}
