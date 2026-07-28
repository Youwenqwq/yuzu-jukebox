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
	now := nowMs()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO room_player_bindings (player_id, room_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(player_id) DO UPDATE SET
			room_id = excluded.room_id,
			updated_at = excluded.updated_at`,
		playerID, roomID, now, now)
	if err != nil {
		return RoomPlayerBinding{}, err
	}
	return s.GetRoomPlayerBindingByPlayer(ctx, playerID)
}

func (s *Store) ListRoomPlayerBindings(ctx context.Context, roomID string) ([]RoomPlayerBinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT room_id, player_id, created_at, updated_at
		 FROM room_player_bindings WHERE room_id = ? ORDER BY player_id`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []RoomPlayerBinding
	for rows.Next() {
		binding, err := scanRoomPlayerBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
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

type RoomOutputState struct {
	RoomID    string
	Volume    int
	UpdatedAt int64
}

func (s *Store) GetRoomOutputState(ctx context.Context, roomID string) (RoomOutputState, error) {
	var state RoomOutputState
	err := s.db.QueryRowContext(ctx,
		`SELECT room_id, volume, updated_at FROM room_output_state WHERE room_id = ?`,
		roomID,
	).Scan(&state.RoomID, &state.Volume, &state.UpdatedAt)
	return state, err
}

func (s *Store) SetRoomOutputVolume(ctx context.Context, roomID string, volume int) (RoomOutputState, error) {
	now := nowMs()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO room_output_state (room_id, volume, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(room_id) DO UPDATE SET
			volume = excluded.volume,
			updated_at = excluded.updated_at`,
		roomID, volume, now)
	if err != nil {
		return RoomOutputState{}, err
	}
	return s.GetRoomOutputState(ctx, roomID)
}
