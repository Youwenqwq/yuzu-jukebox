package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	db *sql.DB
}

func nowMs() int64 { return time.Now().UnixMilli() }

// Open 打开（必要时创建）SQLite 数据库并执行迁移。
func Open(path string) (*Store, error) {
	// WAL：读写不互斥；busy_timeout：写锁竞争时等待而非立刻报错；
	// foreign_keys：SQLite 默认不开外键约束。
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite 同时只能一个写者；让 Go 层排队比暴露 SQLITE_BUSY 好。
	db.SetMaxOpenConns(1)

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return nil, err
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接（测试与运维脚本用）。
func (s *Store) DB() *sql.DB { return s.db }

// ---------- 房间 ----------

type Room struct {
	ID           string
	Name         string
	PasswordHash string
	PolicyJSON   string
	CreatedAt    int64
}

func (s *Store) CreateRoom(ctx context.Context, r Room) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rooms (id, name, guest_password_hash, policy_json, created_at) VALUES (?,?,?,?,?)`,
		r.ID, r.Name, r.PasswordHash, r.PolicyJSON, r.CreatedAt)
	return err
}

func (s *Store) UpdateRoom(ctx context.Context, id, name, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE rooms SET name = ?, guest_password_hash = ? WHERE id = ?`, name, passwordHash, id)
	return err
}

func (s *Store) GetRoom(ctx context.Context, id string) (Room, error) {
	var r Room
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, guest_password_hash, policy_json, created_at FROM rooms WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.PasswordHash, &r.PolicyJSON, &r.CreatedAt)
	return r, err
}

func (s *Store) ListRooms(ctx context.Context) ([]Room, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, guest_password_hash, policy_json, created_at FROM rooms ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Room
	for rows.Next() {
		var r Room
		if err := rows.Scan(&r.ID, &r.Name, &r.PasswordHash, &r.PolicyJSON, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- 队列持久化 ----------

type QueueRow struct {
	EntryID     string
	TrackRef    string
	Title       string
	Artist      string
	DurationMs  int64
	RequestedBy string
	AddedAt     int64
}

// ReplaceQueue 全量重写某房间的队列（队列规模小，重写最不易错）。
func (s *Store) ReplaceQueue(ctx context.Context, roomID string, rows []QueueRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM room_queue WHERE room_id = ?`, roomID); err != nil {
		return err
	}
	for i, r := range rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO room_queue (room_id, ord, entry_id, track_ref, title, artist, duration_ms, requested_by, added_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			roomID, i, r.EntryID, r.TrackRef, r.Title, r.Artist, r.DurationMs, r.RequestedBy, r.AddedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadQueue(ctx context.Context, roomID string) ([]QueueRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT entry_id, track_ref, title, artist, duration_ms, requested_by, added_at
		 FROM room_queue WHERE room_id = ? ORDER BY ord`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueRow
	for rows.Next() {
		var r QueueRow
		if err := rows.Scan(&r.EntryID, &r.TrackRef, &r.Title, &r.Artist, &r.DurationMs, &r.RequestedBy, &r.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- 播放历史 ----------

func (s *Store) AddPlayHistory(ctx context.Context, roomID, trackRef, title, requestedBy string, startedAt, endedAt int64, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO play_history (room_id, track_ref, title, requested_by, started_at, ended_at, end_reason)
		 VALUES (?,?,?,?,?,?,?)`,
		roomID, trackRef, title, requestedBy, startedAt, endedAt, reason)
	return err
}

// ---------- 审计 ----------

func (s *Store) Audit(ctx context.Context, actorID, action, target, detailJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (actor_id, action, target, detail_json, created_at) VALUES (?,?,?,?,?)`,
		actorID, action, target, detailJSON, nowMs())
	return err
}

// ---------- 媒体文件（local provider） ----------

type MediaFile struct {
	ID         string
	Filename   string
	Title      string
	Artist     string
	DurationMs int64
	SizeBytes  int64
	UploadedBy string
	CreatedAt  int64
}

func (s *Store) AddMediaFile(ctx context.Context, m MediaFile) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO media_files (id, filename, title, artist, duration_ms, size_bytes, uploaded_by, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		m.ID, m.Filename, m.Title, m.Artist, m.DurationMs, m.SizeBytes, m.UploadedBy, m.CreatedAt)
	return err
}

func (s *Store) GetMediaFile(ctx context.Context, id string) (MediaFile, error) {
	var m MediaFile
	err := s.db.QueryRowContext(ctx,
		`SELECT id, filename, title, artist, duration_ms, size_bytes, uploaded_by, created_at
		 FROM media_files WHERE id = ?`, id).
		Scan(&m.ID, &m.Filename, &m.Title, &m.Artist, &m.DurationMs, &m.SizeBytes, &m.UploadedBy, &m.CreatedAt)
	return m, err
}

func (s *Store) SearchMediaFiles(ctx context.Context, query string, limit int) ([]MediaFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, filename, title, artist, duration_ms, size_bytes, uploaded_by, created_at
		 FROM media_files WHERE title LIKE ? OR artist LIKE ? ORDER BY created_at DESC LIMIT ?`,
		"%"+query+"%", "%"+query+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaFile
	for rows.Next() {
		var m MediaFile
		if err := rows.Scan(&m.ID, &m.Filename, &m.Title, &m.Artist, &m.DurationMs, &m.SizeBytes, &m.UploadedBy, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- 缓存索引 ----------

type CacheRow struct {
	TrackRef       string `json:"track_ref"`
	FilePath       string `json:"file_path"`
	SizeBytes      int64  `json:"size_bytes"`
	LastAccessedAt int64  `json:"last_accessed_at"`
	CreatedAt      int64  `json:"created_at"`
}

func (s *Store) GetCacheRow(ctx context.Context, trackRef string) (CacheRow, error) {
	var c CacheRow
	err := s.db.QueryRowContext(ctx,
		`SELECT track_ref, file_path, size_bytes, last_accessed_at, created_at FROM media_cache WHERE track_ref = ?`,
		trackRef).Scan(&c.TrackRef, &c.FilePath, &c.SizeBytes, &c.LastAccessedAt, &c.CreatedAt)
	return c, err
}

func (s *Store) PutCacheRow(ctx context.Context, c CacheRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO media_cache (track_ref, file_path, size_bytes, last_accessed_at, created_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(track_ref) DO UPDATE SET file_path=excluded.file_path, size_bytes=excluded.size_bytes,
		 last_accessed_at=excluded.last_accessed_at`,
		c.TrackRef, c.FilePath, c.SizeBytes, c.LastAccessedAt, c.CreatedAt)
	return err
}

func (s *Store) TouchCacheRow(ctx context.Context, trackRef string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE media_cache SET last_accessed_at = ? WHERE track_ref = ?`, nowMs(), trackRef)
	return err
}

func (s *Store) DeleteCacheRow(ctx context.Context, trackRef string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM media_cache WHERE track_ref = ?`, trackRef)
	return err
}

// ---------- 凭据 ---------

// GetCredential 取某 provider 的凭据原文；未设置返回空串。
func (s *Store) GetCredential(ctx context.Context, providerID string) (string, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		`SELECT payload FROM credentials WHERE provider = ? ORDER BY id DESC LIMIT 1`,
		providerID).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return payload, err
}

// UpsertCredential 写入凭据并记录校验状态。
// v1 明文存储——加密需要引入密钥管理，待凭据种类变多时再做。
func (s *Store) UpsertCredential(ctx context.Context, providerID, payload, status string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials (provider, payload, status, last_check_at) VALUES (?,?,?,?)`,
		providerID, payload, status, nowMs())
	return err
}

func (s *Store) ListCacheRows(ctx context.Context) ([]CacheRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT track_ref, file_path, size_bytes, last_accessed_at, created_at FROM media_cache
		 ORDER BY last_accessed_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheRow
	for rows.Next() {
		var c CacheRow
		if err := rows.Scan(&c.TrackRef, &c.FilePath, &c.SizeBytes, &c.LastAccessedAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
