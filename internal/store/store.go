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

// UpdateRoomPolicy 更新房间策略 JSON（策略内容已由 room 包校验）。
func (s *Store) UpdateRoomPolicy(ctx context.Context, id, policyJSON string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET policy_json = ? WHERE id = ?`, policyJSON, id)
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

// PlayHistoryRow 一条播放历史。
type PlayHistoryRow struct {
	TrackRef    string `json:"track_ref"`
	Title       string `json:"title"`
	RequestedBy string `json:"requested_by"`
	StartedAt   int64  `json:"started_at"`
	EndedAt     int64  `json:"ended_at"`
	EndReason   string `json:"end_reason"`
}

// PlayHistory 房间的播放历史，最新在前。
func (s *Store) PlayHistory(ctx context.Context, roomID string, offset, limit int) ([]PlayHistoryRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT track_ref, title, requested_by, started_at, ended_at, end_reason
		 FROM play_history WHERE room_id = ? ORDER BY started_at DESC LIMIT ? OFFSET ?`,
		roomID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayHistoryRow{}
	for rows.Next() {
		var r PlayHistoryRow
		if err := rows.Scan(&r.TrackRef, &r.Title, &r.RequestedBy, &r.StartedAt, &r.EndedAt, &r.EndReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TrackStat 单曲目聚合统计（"考古"视角：首播、播放次数、最近播放）。
type TrackStat struct {
	TrackRef      string `json:"track_ref"`
	Title         string `json:"title"`
	PlayCount     int    `json:"play_count"`
	FirstPlayedAt int64  `json:"first_played_at"`
	LastPlayedAt  int64  `json:"last_played_at"`
}

// PlayStats 房间曲目热度榜，按播放次数降序（次数相同按最近播放降序）。
// SQLite 特性：GROUP BY + MAX 聚合时，裸列取自最大行，故 title 为最近一次播放的标题。
func (s *Store) PlayStats(ctx context.Context, roomID string, limit int) ([]TrackStat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT track_ref, title, COUNT(*) AS c, MIN(started_at), MAX(started_at)
		 FROM play_history WHERE room_id = ? GROUP BY track_ref
		 ORDER BY c DESC, MAX(started_at) DESC LIMIT ?`,
		roomID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrackStat{}
	for rows.Next() {
		var t TrackStat
		if err := rows.Scan(&t.TrackRef, &t.Title, &t.PlayCount, &t.FirstPlayedAt, &t.LastPlayedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
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

// ---------- 歌单 ----------

type Playlist struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	TrackCount  int    `json:"track_count"`
}

type PlaylistItem struct {
	Ord        int    `json:"ord"`
	TrackRef   string `json:"track_ref"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMs int64  `json:"duration_ms"`
	AddedAt    int64  `json:"added_at"`
}

func (s *Store) CreatePlaylist(ctx context.Context, p Playlist) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO playlists (id, name, description, created_by, created_at, updated_at)
		 VALUES (?,?,?,?,?,?)`,
		p.ID, p.Name, p.Description, p.CreatedBy, p.CreatedAt, p.UpdatedAt)
	return err
}

func (s *Store) DeletePlaylist(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, id)
	return err
}

func (s *Store) GetPlaylist(ctx context.Context, id string) (Playlist, error) {
	var p Playlist
	err := s.db.QueryRowContext(ctx,
		`SELECT p.id, p.name, p.description, p.created_by, p.created_at, p.updated_at,
		        (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id)
		 FROM playlists p WHERE p.id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.TrackCount)
	return p, err
}

func (s *Store) ListPlaylists(ctx context.Context) ([]Playlist, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.name, p.description, p.created_by, p.created_at, p.updated_at,
		        (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id)
		 FROM playlists p ORDER BY p.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Playlist
	for rows.Next() {
		var p Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.TrackCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AppendPlaylistItems 事务批量追加（ord 从当前最大值续排）。
func (s *Store) AppendPlaylistItems(ctx context.Context, playlistID string, items []PlaylistItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var maxOrd sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(ord) FROM playlist_items WHERE playlist_id = ?`, playlistID).Scan(&maxOrd); err != nil {
		return err
	}
	ord := maxOrd.Int64 + 1
	for _, it := range items {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO playlist_items (playlist_id, ord, track_ref, title, artist, duration_ms, added_at)
			 VALUES (?,?,?,?,?,?,?)`,
			playlistID, ord, it.TrackRef, it.Title, it.Artist, it.DurationMs, it.AddedAt); err != nil {
			return err
		}
		ord++
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlists SET updated_at = ? WHERE id = ?`, nowMs(), playlistID); err != nil {
		return err
	}
	return tx.Commit()
}

// PlaylistItems 分页读取（按 ord 升序）。
func (s *Store) PlaylistItems(ctx context.Context, playlistID string, offset, limit int) ([]PlaylistItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ord, track_ref, title, artist, duration_ms, added_at
		 FROM playlist_items WHERE playlist_id = ? ORDER BY ord LIMIT ? OFFSET ?`,
		playlistID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlaylistItem
	for rows.Next() {
		var it PlaylistItem
		if err := rows.Scan(&it.Ord, &it.TrackRef, &it.Title, &it.Artist, &it.DurationMs, &it.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// DeletePlaylistItem 按 ord 删除一条并重排后续 ord（低频操作，重写换简单）。
func (s *Store) DeletePlaylistItem(ctx context.Context, playlistID string, ord int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM playlist_items WHERE playlist_id = ? AND ord = ?`, playlistID, ord)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlist_items SET ord = ord - 1 WHERE playlist_id = ? AND ord > ?`,
		playlistID, ord); err != nil {
		return err
	}
	return tx.Commit()
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

// UpdateCredentialStatus 更新最新一条凭据记录的校验状态与时间戳（健康检查用）。
func (s *Store) UpdateCredentialStatus(ctx context.Context, providerID, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE credentials SET status = ?, last_check_at = ?
		 WHERE id = (SELECT id FROM credentials WHERE provider = ? ORDER BY id DESC LIMIT 1)`,
		status, nowMs(), providerID)
	return err
}

// GetCredentialStatus 返回最新一条凭据记录的状态；未设置返回 "unset"。
func (s *Store) GetCredentialStatus(ctx context.Context, providerID string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM credentials WHERE provider = ? ORDER BY id DESC LIMIT 1`,
		providerID).Scan(&status)
	if err == sql.ErrNoRows {
		return "unset", nil
	}
	return status, err
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
