package edgeonepublisher

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type UploadState struct {
	LeaseID        string
	TrackRef       string
	Owner          string
	ExpiresAt      int64
	Status         string
	TempPath       string
	ContentVersion string
	Locator        string
	SizeBytes      int64
	ContentType    string
	LastError      string
	UpdatedAt      int64
}

type State struct {
	db  *sql.DB
	dir string
}

func OpenState(path string) (*State, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS uploads (
		lease_id        TEXT PRIMARY KEY,
		track_ref       TEXT NOT NULL,
		owner           TEXT NOT NULL,
		expires_at      INTEGER NOT NULL,
		status          TEXT NOT NULL,
		temp_path       TEXT NOT NULL DEFAULT '',
		content_version TEXT NOT NULL DEFAULT '',
		locator         TEXT NOT NULL DEFAULT '',
		size_bytes      INTEGER NOT NULL DEFAULT 0,
		content_type    TEXT NOT NULL DEFAULT '',
		last_error      TEXT NOT NULL DEFAULT '',
		updated_at      INTEGER NOT NULL
	)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	state := &State{db: db, dir: dir}
	if err := state.cleanupExpired(context.Background(), time.Now().UnixMilli()); err != nil {
		db.Close()
		return nil, err
	}
	return state, nil
}

func (s *State) Close() error { return s.db.Close() }

func (s *State) ObjectDir() string { return filepath.Join(s.dir, "edgeone-objects") }

func (s *State) Put(ctx context.Context, state UploadState) error {
	state.UpdatedAt = time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO uploads
		 (lease_id, track_ref, owner, expires_at, status, temp_path,
		  content_version, locator, size_bytes, content_type, last_error, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(lease_id) DO UPDATE SET
		 status = excluded.status,
		 temp_path = excluded.temp_path,
		 content_version = excluded.content_version,
		 locator = excluded.locator,
		 size_bytes = excluded.size_bytes,
		 content_type = excluded.content_type,
		 last_error = excluded.last_error,
		 updated_at = excluded.updated_at`,
		state.LeaseID, state.TrackRef, state.Owner, state.ExpiresAt, state.Status,
		state.TempPath, state.ContentVersion, state.Locator, state.SizeBytes,
		state.ContentType, state.LastError, state.UpdatedAt)
	return err
}

func (s *State) List(ctx context.Context) ([]UploadState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT lease_id, track_ref, owner, expires_at,
		status, temp_path, content_version, locator, size_bytes, content_type,
		last_error, updated_at FROM uploads ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var states []UploadState
	for rows.Next() {
		var state UploadState
		if err := rows.Scan(&state.LeaseID, &state.TrackRef, &state.Owner,
			&state.ExpiresAt, &state.Status, &state.TempPath,
			&state.ContentVersion, &state.Locator, &state.SizeBytes,
			&state.ContentType, &state.LastError, &state.UpdatedAt); err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return states, nil
}

func (s *State) Delete(ctx context.Context, leaseID string) error {
	var tempPath string
	err := s.db.QueryRowContext(ctx, `SELECT temp_path FROM uploads WHERE lease_id = ?`, leaseID).Scan(&tempPath)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if tempPath != "" {
		_ = os.Remove(tempPath)
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM uploads WHERE lease_id = ?`, leaseID)
	return err
}

func (s *State) cleanupExpired(ctx context.Context, now int64) error {
	rows, err := s.db.QueryContext(ctx, `SELECT lease_id, temp_path FROM uploads WHERE expires_at <= ?`, now)
	if err != nil {
		return err
	}
	var leaseIDs []string
	for rows.Next() {
		var leaseID, tempPath string
		if err := rows.Scan(&leaseID, &tempPath); err != nil {
			rows.Close()
			return err
		}
		leaseIDs = append(leaseIDs, leaseID)
		if tempPath != "" {
			_ = os.Remove(tempPath)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, leaseID := range leaseIDs {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM uploads WHERE lease_id = ?`, leaseID); err != nil {
			return err
		}
	}
	return nil
}
