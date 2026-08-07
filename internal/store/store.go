package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pressly/goose/v3"

	_ "github.com/youwenqwq/yuzu-jukebox/internal/store/migrations"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	db     *sql.DB
	cipher *aesCipher // nil = 明文（未配置 secret_key）
}

func nowMs() int64 { return time.Now().UnixMilli() }

// Open 打开（必要时创建）SQLite 数据库并执行迁移。
func Open(path string, secretKey []byte) (*Store, error) {
	// WAL：读写不互斥；busy_timeout：写锁竞争时等待而非立刻报错；
	// foreign_keys：SQLite 默认不开外键约束。
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
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
	s := &Store{db: db}
	if len(secretKey) > 0 {
		c, err := newAESCipher(secretKey)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("secret key: %w", err)
		}
		s.cipher = c
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接（测试与运维脚本用）。
func (s *Store) DB() *sql.DB { return s.db }

// ---------- 房间 ----------

type Room struct {
	ID                string
	Name              string
	PasswordHash      string
	AccessMode        string
	CodePeriodSeconds int64
	TrustedRoles      []string
	PolicyJSON        string
	CreatedAt         int64
}

func (s *Store) CreateRoom(ctx context.Context, r Room) error {
	if r.AccessMode == "" {
		r.AccessMode = "open"
		if r.PasswordHash != "" {
			r.AccessMode = "static_password"
		}
	}
	if r.CodePeriodSeconds == 0 {
		r.CodePeriodSeconds = 86400
	}
	trustedRoles := r.TrustedRoles
	if trustedRoles == nil {
		trustedRoles = []string{}
	}
	trustedRolesJSON, err := json.Marshal(trustedRoles)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO rooms
		 (id, name, guest_password_hash, guest_access_mode, guest_code_period_seconds, trusted_roles_json, policy_json, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		r.ID, r.Name, r.PasswordHash, r.AccessMode, r.CodePeriodSeconds, string(trustedRolesJSON), r.PolicyJSON, r.CreatedAt)
	return err
}

func (s *Store) UpdateRoom(ctx context.Context, r Room) error {
	trustedRoles := r.TrustedRoles
	if trustedRoles == nil {
		trustedRoles = []string{}
	}
	trustedRolesJSON, err := json.Marshal(trustedRoles)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE rooms
		 SET name = ?, guest_password_hash = ?, guest_access_mode = ?, guest_code_period_seconds = ?,
		     trusted_roles_json = ?
		 WHERE id = ?`,
		r.Name, r.PasswordHash, r.AccessMode, r.CodePeriodSeconds, string(trustedRolesJSON), r.ID)
	return err
}

// UpdateRoomPolicy 更新房间策略 JSON（策略内容已由 room 包校验）。
func (s *Store) UpdateRoomPolicy(ctx context.Context, id, policyJSON string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET policy_json = ? WHERE id = ?`, policyJSON, id)
	return err
}

func (s *Store) GetRoom(ctx context.Context, id string) (Room, error) {
	return scanRoom(s.db.QueryRowContext(ctx,
		`SELECT id, name, guest_password_hash, guest_access_mode, guest_code_period_seconds,
		        trusted_roles_json, policy_json, created_at
		 FROM rooms WHERE id = ?`, id))
}

func (s *Store) ListRooms(ctx context.Context) ([]Room, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, guest_password_hash, guest_access_mode, guest_code_period_seconds,
		        trusted_roles_json, policy_json, created_at
		 FROM rooms ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Room
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type roomScanner interface {
	Scan(dest ...any) error
}

func scanRoom(row roomScanner) (Room, error) {
	var r Room
	var trustedRolesJSON string
	err := row.Scan(
		&r.ID, &r.Name, &r.PasswordHash, &r.AccessMode, &r.CodePeriodSeconds,
		&trustedRolesJSON, &r.PolicyJSON, &r.CreatedAt,
	)
	if err != nil {
		return Room{}, err
	}
	if err := json.Unmarshal([]byte(trustedRolesJSON), &r.TrustedRoles); err != nil {
		return Room{}, fmt.Errorf("decode room trusted roles: %w", err)
	}
	if r.TrustedRoles == nil {
		r.TrustedRoles = []string{}
	}
	return r, nil
}

// ---------- 全局主体与 Room 授权 ----------

type Principal struct {
	ID          string
	Name        string
	Avatar      string // OIDC picture claim 快照；guest/password 恒为空
	Kind        string
	OIDCSubject string
	Roles       []string
	Active      bool
	CreatedAt   int64
	UpdatedAt   int64
}

func (s *Store) UpsertPrincipal(ctx context.Context, p Principal) error {
	roles := p.Roles
	if roles == nil {
		roles = []string{}
	}
	rolesJSON, err := json.Marshal(roles)
	if err != nil {
		return err
	}
	now := nowMs()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users
			(id, kind, name, avatar, oidc_subject, roles_json, active, created_at, updated_at)
		 VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			kind = excluded.kind,
			name = excluded.name,
			avatar = excluded.avatar,
			oidc_subject = COALESCE(NULLIF(users.oidc_subject, ''), excluded.oidc_subject),
			roles_json = excluded.roles_json,
			active = excluded.active,
			updated_at = excluded.updated_at`,
		p.ID, p.Kind, p.Name, p.Avatar, p.OIDCSubject, string(rolesJSON), p.Active, now, now)
	return err
}

func (s *Store) GetPrincipal(ctx context.Context, id string) (Principal, error) {
	return scanPrincipal(s.db.QueryRowContext(ctx,
		`SELECT id, name, avatar, kind, COALESCE(oidc_subject, ''), roles_json, active, created_at, updated_at
		 FROM users WHERE id = ?`, id))
}

func (s *Store) GetPrincipalByOIDCSubject(ctx context.Context, subject string) (Principal, error) {
	return scanPrincipal(s.db.QueryRowContext(ctx,
		`SELECT id, name, avatar, kind, COALESCE(oidc_subject, ''), roles_json, active, created_at, updated_at
		 FROM users WHERE oidc_subject = ?`, subject))
}

// ListPrincipals returns principals whose ID or name contains query.
// Results are ordered by ID and capped at 100 rows.
func (s *Store) ListPrincipals(ctx context.Context, query string, limit int) ([]Principal, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	query = strings.TrimSpace(query)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, avatar, kind, COALESCE(oidc_subject, ''), roles_json, active, created_at, updated_at
		 FROM users
		 WHERE ? = ''
		    OR instr(lower(id), lower(?)) > 0
		    OR instr(lower(name), lower(?)) > 0
		 ORDER BY id
		 LIMIT ?`,
		query, query, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	principals := make([]Principal, 0)
	for rows.Next() {
		principal, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		principals = append(principals, principal)
	}
	return principals, rows.Err()
}

type principalScanner interface {
	Scan(dest ...any) error
}

func scanPrincipal(row principalScanner) (Principal, error) {
	var p Principal
	var rolesJSON string
	err := row.Scan(
		&p.ID,
		&p.Name,
		&p.Avatar,
		&p.Kind,
		&p.OIDCSubject,
		&rolesJSON,
		&p.Active,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if err != nil {
		return Principal{}, err
	}
	if err := json.Unmarshal([]byte(rolesJSON), &p.Roles); err != nil {
		return Principal{}, fmt.Errorf("decode principal roles: %w", err)
	}
	return p, nil
}

type ExternalIdentityLink struct {
	IntegrationID string
	AdapterID     string
	ScopeType     string
	ScopeID       string
	SubjectID     string
	PrincipalID   string
}

func (s *Store) UpsertExternalIdentityLink(
	ctx context.Context,
	integrationID, adapterID, scopeType, scopeID, subjectID, principalID string,
) error {
	now := nowMs()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO external_identity_links
			(integration_id, adapter_id, scope_type, scope_id, subject_id, principal_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(integration_id, adapter_id, scope_type, scope_id, subject_id)
		 DO UPDATE SET principal_id = excluded.principal_id, updated_at = excluded.updated_at`,
		integrationID, adapterID, scopeType, scopeID, subjectID, principalID, now, now)
	return err
}

func (s *Store) ResolveExternalIdentityLink(
	ctx context.Context,
	integrationID, adapterID, scopeType, scopeID, subjectID string,
) (string, error) {
	var principalID string
	err := s.db.QueryRowContext(ctx,
		`SELECT principal_id FROM external_identity_links
		 WHERE integration_id = ? AND adapter_id = ? AND scope_type = ? AND scope_id = ? AND subject_id = ?`,
		integrationID, adapterID, scopeType, scopeID, subjectID).Scan(&principalID)
	return principalID, err
}

func (s *Store) RemoveExternalIdentityLink(
	ctx context.Context,
	integrationID, adapterID, scopeType, scopeID, subjectID string,
) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM external_identity_links
		 WHERE integration_id = ? AND adapter_id = ? AND scope_type = ? AND scope_id = ? AND subject_id = ?`,
		integrationID, adapterID, scopeType, scopeID, subjectID)
	return err
}

func (s *Store) ListExternalIdentityLinks(ctx context.Context, integrationID string) ([]ExternalIdentityLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT integration_id, adapter_id, scope_type, scope_id, subject_id, principal_id
		 FROM external_identity_links
		 WHERE integration_id = ?
		 ORDER BY adapter_id, scope_type, scope_id, subject_id, principal_id`,
		integrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := make([]ExternalIdentityLink, 0)
	for rows.Next() {
		var link ExternalIdentityLink
		if err := rows.Scan(
			&link.IntegrationID, &link.AdapterID, &link.ScopeType,
			&link.ScopeID, &link.SubjectID, &link.PrincipalID,
		); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

type ExternalScopeRoom struct {
	IntegrationID string
	AdapterID     string
	ScopeType     string
	ScopeID       string
	RoomID        string
}

func (s *Store) BindExternalScopeRoom(
	ctx context.Context,
	integrationID, adapterID, scopeType, scopeID, roomID string,
) error {
	now := nowMs()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO external_scope_rooms
			(integration_id, adapter_id, scope_type, scope_id, room_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(integration_id, adapter_id, scope_type, scope_id)
		 DO UPDATE SET room_id = excluded.room_id, updated_at = excluded.updated_at`,
		integrationID, adapterID, scopeType, scopeID, roomID, now, now)
	return err
}

func (s *Store) ResolveExternalScopeRoom(
	ctx context.Context,
	integrationID, adapterID, scopeType, scopeID string,
) (string, error) {
	var roomID string
	err := s.db.QueryRowContext(ctx,
		`SELECT room_id FROM external_scope_rooms
		 WHERE integration_id = ? AND adapter_id = ? AND scope_type = ? AND scope_id = ?`,
		integrationID, adapterID, scopeType, scopeID).Scan(&roomID)
	return roomID, err
}

func (s *Store) RemoveExternalScopeRoom(
	ctx context.Context,
	integrationID, adapterID, scopeType, scopeID string,
) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM external_scope_rooms
		 WHERE integration_id = ? AND adapter_id = ? AND scope_type = ? AND scope_id = ?`,
		integrationID, adapterID, scopeType, scopeID)
	return err
}

func (s *Store) ListExternalScopeRooms(ctx context.Context, integrationID string) ([]ExternalScopeRoom, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT integration_id, adapter_id, scope_type, scope_id, room_id
		 FROM external_scope_rooms
		 WHERE integration_id = ?
		 ORDER BY adapter_id, scope_type, scope_id, room_id`,
		integrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := make([]ExternalScopeRoom, 0)
	for rows.Next() {
		var binding ExternalScopeRoom
		if err := rows.Scan(
			&binding.IntegrationID, &binding.AdapterID, &binding.ScopeType,
			&binding.ScopeID, &binding.RoomID,
		); err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

type RoomGrant struct {
	RoomID      string
	PrincipalID string
	Capability  string
	GrantedAt   int64
}

func (s *Store) GrantRoomGrant(ctx context.Context, roomID, principalID, capability string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO room_principal_grants (room_id, principal_id, capability, granted_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(room_id, principal_id, capability)
		 DO UPDATE SET granted_at = excluded.granted_at`,
		roomID, principalID, capability, nowMs())
	return err
}

func (s *Store) RevokeRoomGrant(ctx context.Context, roomID, principalID, capability string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM room_principal_grants
		 WHERE room_id = ? AND principal_id = ? AND capability = ?`,
		roomID, principalID, capability)
	return err
}

func (s *Store) ListRoomGrants(ctx context.Context, roomID string) ([]RoomGrant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT room_id, principal_id, capability, granted_at
		 FROM room_principal_grants
		 WHERE room_id = ?
		 ORDER BY principal_id, capability`,
		roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := make([]RoomGrant, 0)
	for rows.Next() {
		var grant RoomGrant
		if err := rows.Scan(&grant.RoomID, &grant.PrincipalID, &grant.Capability, &grant.GrantedAt); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *Store) HasRoomGrant(
	ctx context.Context,
	roomID, principalID, capability string,
) (bool, error) {
	var granted bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM room_principal_grants
			WHERE room_id = ? AND principal_id = ? AND capability = ?
		)`,
		roomID, principalID, capability).Scan(&granted)
	return granted, err
}

// ---------- 队列持久化 ----------

type QueueRow struct {
	EntryID          string
	TrackRef         string
	Title            string
	Artist           string
	DurationMs       int64
	Album            string
	CoverURL         string
	SourceURL        string
	ContributorsJSON string
	RequestedBy      string
	RequesterName    string
	AddedAt          int64
}

// ReplaceQueue 全量重写某房间的队列（队列规模小，重写最不易错）。
// rows 以当前曲目打头（currentEntryID 非空时），其后是待播条目；游标与队列在
// 同一事务中落库，避免出现「游标指向已不存在的条目」的中间态。
func (s *Store) ReplaceQueue(ctx context.Context, roomID, currentEntryID string, rows []QueueRow) error {
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
			`INSERT INTO room_queue (room_id, ord, entry_id, track_ref, title, artist, duration_ms,
			 album, cover_url, source_url, contributors_json, requested_by, requester_name, added_at, is_current)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			roomID, i, r.EntryID, r.TrackRef, r.Title, r.Artist, r.DurationMs,
			r.Album, r.CoverURL, r.SourceURL, r.ContributorsJSON, r.RequestedBy, r.RequesterName, r.AddedAt,
			currentEntryID != "" && r.EntryID == currentEntryID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadQueue 返回房间的持久队列以及当前曲目的 entry id。currentEntryID 为空表示
// 房间此刻没有在播曲目，rows 全部是待播条目。
func (s *Store) LoadQueue(ctx context.Context, roomID string) ([]QueueRow, string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT entry_id, track_ref, title, artist, duration_ms, album, cover_url, source_url, contributors_json,
		        requested_by, requester_name, added_at, is_current FROM room_queue WHERE room_id = ? ORDER BY ord`, roomID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []QueueRow
	var currentEntryID string
	for rows.Next() {
		var r QueueRow
		var isCurrent bool
		if err := rows.Scan(&r.EntryID, &r.TrackRef, &r.Title, &r.Artist, &r.DurationMs,
			&r.Album, &r.CoverURL, &r.SourceURL, &r.ContributorsJSON, &r.RequestedBy, &r.RequesterName,
			&r.AddedAt, &isCurrent); err != nil {
			return nil, "", err
		}
		if isCurrent {
			currentEntryID = r.EntryID
		}
		out = append(out, r)
	}
	return out, currentEntryID, rows.Err()
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
	RoomID      string `json:"room_id"`
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
		`SELECT room_id, track_ref, title, requested_by, started_at, ended_at, end_reason
		 FROM play_history WHERE room_id = ? ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`,
		roomID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayHistoryRow{}
	for rows.Next() {
		var r PlayHistoryRow
		if err := rows.Scan(&r.RoomID, &r.TrackRef, &r.Title, &r.RequestedBy, &r.StartedAt, &r.EndedAt, &r.EndReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PlayHistoryByRequester 返回点歌人在所有房间的播放历史，最新在前。
func (s *Store) PlayHistoryByRequester(ctx context.Context, requesterID string, offset, limit int) ([]PlayHistoryRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT room_id, track_ref, title, requested_by, started_at, ended_at, end_reason
		 FROM play_history WHERE requested_by = ? ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`,
		requesterID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayHistoryRow{}
	for rows.Next() {
		var r PlayHistoryRow
		if err := rows.Scan(&r.RoomID, &r.TrackRef, &r.Title, &r.RequestedBy, &r.StartedAt, &r.EndedAt, &r.EndReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HotTrack 全局热门条目（跨房间 play_history 聚合）。
type HotTrack struct {
	TrackRef     string `json:"track_ref"`
	Title        string `json:"title"`
	PlayCount    int    `json:"play_count"`
	LastPlayedAt int64  `json:"last_played_at"`
}

// HotTracks 返回全局热门曲目；跳过或播放错误仍代表点播意图，因此不按 end_reason 过滤。
func (s *Store) HotTracks(ctx context.Context, sinceMs int64, limit, offset int) ([]HotTrack, error) {
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`WITH filtered AS (
			SELECT id, track_ref, title, started_at
			FROM play_history
			WHERE ? <= 0 OR started_at >= ?
		)
		SELECT f.track_ref,
		       (SELECT recent.title
		        FROM filtered AS recent
		        WHERE recent.track_ref = f.track_ref
		        ORDER BY recent.started_at DESC, recent.id DESC
		        LIMIT 1),
		       COUNT(*) AS play_count,
		       MAX(f.started_at) AS last_played_at
		FROM filtered AS f
		GROUP BY f.track_ref
		ORDER BY play_count DESC, last_played_at DESC
		LIMIT ? OFFSET ?`,
		sinceMs, sinceMs, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HotTrack{}
	for rows.Next() {
		var t HotTrack
		if err := rows.Scan(&t.TrackRef, &t.Title, &t.PlayCount, &t.LastPlayedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
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

// RoomPrefetchHorizon 返回所有房间从队列游标起前 depth 条曲目的 track_ref，按紧迫度
// 升序去重（越靠近正在播放的位置越紧迫）。room_queue 的队首就是游标：房间在播时是
// 当前曲目本身，空闲时是下一首要放的。
func (s *Store) RoomPrefetchHorizon(ctx context.Context, depth int) ([]string, error) {
	if depth <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT track_ref FROM
		(SELECT track_ref, ROW_NUMBER() OVER (PARTITION BY room_id ORDER BY ord) AS position
		 FROM room_queue)
		WHERE position <= ? GROUP BY track_ref ORDER BY MIN(position), track_ref`, depth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// DeleteRoom 删除房间及其队列与播放历史。
func (s *Store) DeleteRoom(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM room_queue WHERE room_id = ?`,
		`DELETE FROM play_history WHERE room_id = ?`,
		`DELETE FROM rooms WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- 会话持久化 ----------

// SaveSession writes a standard session without an Integration source.
func (s *Store) SaveSession(ctx context.Context, token, identityJSON string, expiresAt int64) error {
	return s.SaveSessionWithActorSource(ctx, token, identityJSON, SessionSource{}, expiresAt)
}

// SessionSource records the trusted Integration scope that issued an actor
// session. Empty fields identify a normal WebUI/CLI session.
type SessionSource struct {
	IntegrationID string
	AdapterID     string
	ScopeType     string
	ScopeID       string
}

// SaveSessionWithSource preserves the Integration-only persistence API used by
// older in-process callers.
func (s *Store) SaveSessionWithSource(
	ctx context.Context,
	token, identityJSON, integrationID string,
	expiresAt int64,
) error {
	return s.SaveSessionWithActorSource(ctx, token, identityJSON, SessionSource{
		IntegrationID: integrationID,
	}, expiresAt)
}

func (s *Store) SaveSessionWithActorSource(
	ctx context.Context,
	token, identityJSON string,
	source SessionSource,
	expiresAt int64,
) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions
			(token, identity_json, expires_at, integration_id,
			 integration_adapter_id, integration_scope_type, integration_scope_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(token) DO UPDATE SET
			identity_json = excluded.identity_json,
			expires_at = excluded.expires_at,
			integration_id = excluded.integration_id,
			integration_adapter_id = excluded.integration_adapter_id,
			integration_scope_type = excluded.integration_scope_type,
			integration_scope_id = excluded.integration_scope_id`,
		token, identityJSON, expiresAt, source.IntegrationID,
		source.AdapterID, source.ScopeType, source.ScopeID)
	return err
}

// SessionRow is a persisted session.
type SessionRow struct {
	Token         string
	IdentityJSON  string
	IntegrationID string
	AdapterID     string
	ScopeType     string
	ScopeID       string
	ExpiresAt     int64
}

// LoadSessions 读出全部未过期会话（启动恢复用）。
func (s *Store) LoadSessions(ctx context.Context, now int64) ([]SessionRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT token, identity_json, integration_id,
		        integration_adapter_id, integration_scope_type, integration_scope_id,
		        expires_at
		 FROM sessions WHERE expires_at > ?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(
			&r.Token, &r.IdentityJSON, &r.IntegrationID,
			&r.AdapterID, &r.ScopeType, &r.ScopeID, &r.ExpiresAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteSession 吊销一条会话。
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}

func (s *Store) DeleteSessionsByIntegration(ctx context.Context, integrationID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE integration_id = ?`, integrationID)
	return err
}

// PruneSessions 清理过期会话（定期调用）。
func (s *Store) PruneSessions(ctx context.Context, now int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now)
	return err
}

// ---------- 审计 ----------

func (s *Store) Audit(ctx context.Context, actorID, action, target, detailJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (actor_id, action, target, detail_json, created_at) VALUES (?,?,?,?,?)`,
		actorID, action, target, detailJSON, nowMs())
	return err
}

// AuditFilter narrows audit-log queries by exact field matches.
type AuditFilter struct {
	ActorID string
	Action  string
	Target  string
}

// AuditEntry is one persisted audit event.
type AuditEntry struct {
	ActorID   string          `json:"actor_id"`
	Action    string          `json:"action"`
	Target    string          `json:"target"`
	Detail    json.RawMessage `json:"detail"`
	CreatedAt int64           `json:"created_at"`
}

// QueryAudit returns audit events newest-first. Paging is bounded here as well
// as at the HTTP boundary so non-HTTP callers cannot accidentally issue an
// unbounded query.
func (s *Store) QueryAudit(ctx context.Context, filter AuditFilter, limit, offset int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	query := `SELECT actor_id, action, target, detail_json, created_at FROM audit_log WHERE 1=1`
	args := make([]any, 0, 5)
	if filter.ActorID != "" {
		query += ` AND actor_id = ?`
		args = append(args, filter.ActorID)
	}
	if filter.Action != "" {
		query += ` AND action = ?`
		args = append(args, filter.Action)
	}
	if filter.Target != "" {
		query += ` AND target = ?`
		args = append(args, filter.Target)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]AuditEntry, 0)
	for rows.Next() {
		var entry AuditEntry
		var detailJSON string
		if err := rows.Scan(&entry.ActorID, &entry.Action, &entry.Target, &detailJSON, &entry.CreatedAt); err != nil {
			return nil, err
		}
		entry.Detail = json.RawMessage(detailJSON)
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ---------- 媒体文件（local provider） ----------

type MediaFile struct {
	ID          string
	Filename    string
	Title       string
	Artist      string
	DurationMs  int64
	SizeBytes   int64
	UploadedBy  string
	Album       string
	CoverPath   string
	BitrateKbps int
	CreatedAt   int64
}

func (s *Store) AddMediaFile(ctx context.Context, m MediaFile) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO media_files (id, filename, title, artist, duration_ms, size_bytes, uploaded_by, album, cover_path, bitrate_kbps, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Filename, m.Title, m.Artist, m.DurationMs, m.SizeBytes, m.UploadedBy,
		m.Album, m.CoverPath, m.BitrateKbps, m.CreatedAt)
	return err
}

func (s *Store) GetMediaFile(ctx context.Context, id string) (MediaFile, error) {
	var m MediaFile
	err := s.db.QueryRowContext(ctx,
		`SELECT id, filename, title, artist, duration_ms, size_bytes, uploaded_by, album, cover_path, bitrate_kbps, created_at
		 FROM media_files WHERE id = ?`, id).
		Scan(&m.ID, &m.Filename, &m.Title, &m.Artist, &m.DurationMs, &m.SizeBytes, &m.UploadedBy,
			&m.Album, &m.CoverPath, &m.BitrateKbps, &m.CreatedAt)
	return m, err
}

func (s *Store) ListMediaFiles(ctx context.Context) ([]MediaFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, filename, title, artist, duration_ms, size_bytes, uploaded_by, album, cover_path, bitrate_kbps, created_at
		 FROM media_files ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MediaFile, 0)
	for rows.Next() {
		var m MediaFile
		if err := rows.Scan(&m.ID, &m.Filename, &m.Title, &m.Artist, &m.DurationMs, &m.SizeBytes, &m.UploadedBy,
			&m.Album, &m.CoverPath, &m.BitrateKbps, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMediaFile(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM media_files WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) SearchMediaFiles(ctx context.Context, query string, limit, offset int) ([]MediaFile, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, filename, title, artist, duration_ms, size_bytes, uploaded_by, album, cover_path, bitrate_kbps, created_at
		 FROM media_files WHERE title LIKE ? OR artist LIKE ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		"%"+query+"%", "%"+query+"%", limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaFile
	for rows.Next() {
		var m MediaFile
		if err := rows.Scan(&m.ID, &m.Filename, &m.Title, &m.Artist, &m.DurationMs, &m.SizeBytes, &m.UploadedBy,
			&m.Album, &m.CoverPath, &m.BitrateKbps, &m.CreatedAt); err != nil {
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
	BitrateKbps    int    `json:"bitrate_kbps"`
	LastAccessedAt int64  `json:"last_accessed_at"`
	CreatedAt      int64  `json:"created_at"`
}

func (s *Store) GetCacheRow(ctx context.Context, trackRef string) (CacheRow, error) {
	var c CacheRow
	err := s.db.QueryRowContext(ctx,
		`SELECT track_ref, file_path, size_bytes, bitrate_kbps, last_accessed_at, created_at FROM media_cache WHERE track_ref = ?`,
		trackRef).Scan(&c.TrackRef, &c.FilePath, &c.SizeBytes, &c.BitrateKbps, &c.LastAccessedAt, &c.CreatedAt)
	return c, err
}

func (s *Store) PutCacheRow(ctx context.Context, c CacheRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO media_cache (track_ref, file_path, size_bytes, bitrate_kbps, last_accessed_at, created_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(track_ref) DO UPDATE SET file_path=excluded.file_path, size_bytes=excluded.size_bytes,
		 bitrate_kbps=excluded.bitrate_kbps,
		 last_accessed_at=excluded.last_accessed_at`,
		c.TrackRef, c.FilePath, c.SizeBytes, c.BitrateKbps, c.LastAccessedAt, c.CreatedAt)
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
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	CreatedBy     string `json:"created_by"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	TrackCount    int    `json:"track_count"`
	BoundProvider string `json:"bound_provider,omitempty"`
	BoundRemoteID string `json:"bound_remote_id,omitempty"`
	LastSyncAt    int64  `json:"last_sync_at,omitempty"`
	LastSyncError string `json:"last_sync_error,omitempty"`
	CoverURL      string `json:"cover_url,omitempty"`
	CoverPath     string `json:"cover_path,omitempty"`
}

type PlaylistItem struct {
	Ord              int    `json:"ord"`
	TrackRef         string `json:"track_ref"`
	Title            string `json:"title"`
	Artist           string `json:"artist"`
	DurationMs       int64  `json:"duration_ms"`
	Album            string `json:"album,omitempty"`
	CoverURL         string `json:"cover_url,omitempty"`
	SourceURL        string `json:"source_url,omitempty"`
	ContributorsJSON string `json:"contributors_json,omitempty"`
	AddedAt          int64  `json:"added_at"`
}

func (s *Store) CreatePlaylist(ctx context.Context, p Playlist) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO playlists
		 (id, name, description, created_by, created_at, updated_at,
		  bound_provider, bound_remote_id, last_sync_at, last_sync_error, cover_url, cover_path)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Description, p.CreatedBy, p.CreatedAt, p.UpdatedAt,
		p.BoundProvider, p.BoundRemoteID, p.LastSyncAt, p.LastSyncError, p.CoverURL, p.CoverPath)
	return err
}

func (s *Store) DeletePlaylist(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, id)
	return err
}

func (s *Store) SetPlaylistCover(ctx context.Context, id, coverURL, coverPath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE playlists SET cover_url = ?, cover_path = ? WHERE id = ?`,
		coverURL, coverPath, id)
	return err
}

func (s *Store) GetPlaylist(ctx context.Context, id string) (Playlist, error) {
	var p Playlist
	err := s.db.QueryRowContext(ctx,
		`SELECT p.id, p.name, p.description, p.created_by, p.created_at, p.updated_at,
		        p.bound_provider, p.bound_remote_id, p.last_sync_at, p.last_sync_error,
		        p.cover_url, p.cover_path,
		        (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id)
		 FROM playlists p WHERE p.id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
			&p.BoundProvider, &p.BoundRemoteID, &p.LastSyncAt, &p.LastSyncError,
			&p.CoverURL, &p.CoverPath, &p.TrackCount)
	return p, err
}

func (s *Store) ListPlaylists(ctx context.Context) ([]Playlist, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.name, p.description, p.created_by, p.created_at, p.updated_at,
		        p.bound_provider, p.bound_remote_id, p.last_sync_at, p.last_sync_error,
		        p.cover_url, p.cover_path,
		        (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id)
		 FROM playlists p ORDER BY p.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Playlist
	for rows.Next() {
		var p Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
			&p.BoundProvider, &p.BoundRemoteID, &p.LastSyncAt, &p.LastSyncError,
			&p.CoverURL, &p.CoverPath, &p.TrackCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPlaylistByBinding(ctx context.Context, providerID, remoteID string) (Playlist, bool, error) {
	var p Playlist
	err := s.db.QueryRowContext(ctx,
		`SELECT p.id, p.name, p.description, p.created_by, p.created_at, p.updated_at,
		        p.bound_provider, p.bound_remote_id, p.last_sync_at, p.last_sync_error,
		        p.cover_url, p.cover_path,
		        (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id)
		 FROM playlists p WHERE p.bound_provider = ? AND p.bound_remote_id = ?`,
		providerID, remoteID).
		Scan(&p.ID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
			&p.BoundProvider, &p.BoundRemoteID, &p.LastSyncAt, &p.LastSyncError,
			&p.CoverURL, &p.CoverPath, &p.TrackCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Playlist{}, false, nil
	}
	if err != nil {
		return Playlist{}, false, err
	}
	return p, true, nil
}

func (s *Store) ListBoundPlaylists(ctx context.Context) ([]Playlist, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.name, p.description, p.created_by, p.created_at, p.updated_at,
		        p.bound_provider, p.bound_remote_id, p.last_sync_at, p.last_sync_error,
		        (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id)
		 FROM playlists p WHERE p.bound_provider != '' ORDER BY p.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Playlist
	for rows.Next() {
		var p Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
			&p.BoundProvider, &p.BoundRemoteID, &p.LastSyncAt, &p.LastSyncError, &p.TrackCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReplacePlaylistItems 在单个事务内全量替换歌单条目，序号从 0 连续排列。
func (s *Store) ReplacePlaylistItems(ctx context.Context, playlistID string, items []PlaylistItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_items WHERE playlist_id = ?`, playlistID); err != nil {
		return err
	}
	for ord, it := range items {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO playlist_items (playlist_id, ord, track_ref, title, artist, duration_ms,
			 album, cover_url, source_url, contributors_json, added_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			playlistID, ord, it.TrackRef, it.Title, it.Artist, it.DurationMs,
			it.Album, it.CoverURL, it.SourceURL, it.ContributorsJSON, it.AddedAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlists SET updated_at = ? WHERE id = ?`, nowMs(), playlistID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetPlaylistSyncResult(ctx context.Context, id, name, coverURL string, at int64, syncErr error) error {
	if syncErr != nil {
		_, err := s.db.ExecContext(ctx,
			`UPDATE playlists SET last_sync_at = ?, last_sync_error = ? WHERE id = ?`,
			at, syncErr.Error(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE playlists
		 SET last_sync_at = ?, last_sync_error = '',
		     name = CASE WHEN ? != '' THEN ? ELSE name END,
		     cover_url = CASE WHEN ? != '' THEN ? ELSE cover_url END,
		     cover_path = ''
		 WHERE id = ?`,
		at, name, name, coverURL, coverURL, id)
	return err
}

// ClearPlaylistBinding 解除外部绑定，保留当前歌单名称和条目。
func (s *Store) ClearPlaylistBinding(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE playlists
		 SET bound_provider = '', bound_remote_id = '', last_sync_at = 0, last_sync_error = ''
		 WHERE id = ?`, id)
	return err
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
			`INSERT INTO playlist_items (playlist_id, ord, track_ref, title, artist, duration_ms,
			 album, cover_url, source_url, contributors_json, added_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			playlistID, ord, it.TrackRef, it.Title, it.Artist, it.DurationMs,
			it.Album, it.CoverURL, it.SourceURL, it.ContributorsJSON, it.AddedAt); err != nil {
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
		`SELECT ord, track_ref, title, artist, duration_ms, album, cover_url, source_url, contributors_json, added_at
		 FROM playlist_items WHERE playlist_id = ? ORDER BY ord LIMIT ? OFFSET ?`,
		playlistID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlaylistItem
	for rows.Next() {
		var it PlaylistItem
		if err := rows.Scan(&it.Ord, &it.TrackRef, &it.Title, &it.Artist, &it.DurationMs,
			&it.Album, &it.CoverURL, &it.SourceURL, &it.ContributorsJSON, &it.AddedAt); err != nil {
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

// MovePlaylistItem 把 ord 处的条目移动到 toOrd（clamp 到 [1, len]），
// 事务内完成，重排后序号保持 1-based 连续。ord 超界返回 sql.ErrNoRows。
// 返回最终落位序号。
func (s *Store) MovePlaylistItem(ctx context.Context, playlistID string, ord, toOrd int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// (playlist_id, ord) 有唯一约束，区间平移会逐行撞约束；
	// 与 Delete 同哲学：读出序号序列，在内存里重排，再整体重写。
	rows, err := tx.QueryContext(ctx,
		`SELECT ord FROM playlist_items WHERE playlist_id = ? ORDER BY ord`, playlistID)
	if err != nil {
		return 0, err
	}
	var ords []int
	for rows.Next() {
		var o int
		if err := rows.Scan(&o); err != nil {
			rows.Close()
			return 0, err
		}
		ords = append(ords, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	n := len(ords)
	if ord < 1 || ord > n {
		return 0, sql.ErrNoRows
	}
	if toOrd < 1 {
		toOrd = 1
	}
	if toOrd > n {
		toOrd = n
	}
	if ord == toOrd {
		return toOrd, tx.Commit()
	}

	moved := ords[ord-1]
	rest := append(ords[:ord-1:ord-1], ords[ord:]...)
	rest = append(rest[:toOrd-1], append([]int{moved}, rest[toOrd-1:]...)...)

	// 先全部取负腾出空间，再按新顺序落位。
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlist_items SET ord = -ord WHERE playlist_id = ?`, playlistID); err != nil {
		return 0, err
	}
	for newOrd, oldOrd := range rest {
		if _, err := tx.ExecContext(ctx,
			`UPDATE playlist_items SET ord = ? WHERE playlist_id = ? AND ord = ?`,
			newOrd+1, playlistID, -oldOrd); err != nil {
			return 0, err
		}
	}
	return toOrd, tx.Commit()
}

// ---------- 凭据 ---------

// GetCredential 取某 provider 的凭据原文；未设置返回空串。
// 存储层自动解密；历史明文行（无 enc1: 前缀）原样返回。
func (s *Store) GetCredential(ctx context.Context, providerID string) (string, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		`SELECT payload FROM credentials WHERE provider = ? ORDER BY id DESC LIMIT 1`,
		providerID).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return s.decrypt(payload)
}

// UpsertCredential 写入凭据并记录校验状态。
// 配置了 secret_key 时 AES-GCM 加密落盘（enc1: 前缀）。
// 新行继承上一条记录的 owner/account 绑定：凭据轮换不是重新委托，
// 重绑只通过 SetCredentialOwner 显式发生（API 层，人设凭据时）。
func (s *Store) UpsertCredential(ctx context.Context, providerID, payload, status string) error {
	enc, err := s.encrypt(payload)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials (provider, payload, status, last_check_at,
			owner_principal_id, account_uid, account_name, account_avatar)
		 SELECT ?, ?, ?, ?,
			owner_principal_id, account_uid, account_name, account_avatar
		 FROM credentials WHERE provider = ? ORDER BY id DESC LIMIT 1`,
		providerID, enc, status, nowMs(), providerID)
	if err != nil {
		return err
	}
	// INSERT...SELECT 匹配不到上一行时静默插入 0 行：首个凭据走纯 INSERT，
	// owner/account 列取默认值（未绑定）。
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO credentials (provider, payload, status, last_check_at) VALUES (?,?,?,?)`,
			providerID, enc, status, nowMs())
	}
	return err
}

func (s *Store) encrypt(plain string) (string, error) {
	if s.cipher == nil || plain == "" {
		return plain, nil
	}
	return s.cipher.encrypt(plain)
}

func (s *Store) decrypt(stored string) (string, error) {
	if s.cipher == nil {
		if strings.HasPrefix(stored, "enc1:") {
			return "", errors.New("credential encrypted but no secret_key configured")
		}
		return stored, nil
	}
	return s.cipher.decrypt(stored)
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

// AccountProfile 是凭据对应平台账号的资料快照（provider 校验凭据时写入）。
type AccountProfile struct {
	UID    string
	Name   string
	Avatar string
}

// CredentialOwner 是最新凭据行的归属信息。
// PrincipalID 为空 = 未绑定（无写操作权限）；ok=false = 该 provider 无凭据行。
type CredentialOwner struct {
	PrincipalID string
	Account     AccountProfile
}

// GetCredentialOwner 读最新凭据行的 owner 绑定与账号资料。
func (s *Store) GetCredentialOwner(ctx context.Context, providerID string) (CredentialOwner, bool, error) {
	var o CredentialOwner
	err := s.db.QueryRowContext(ctx,
		`SELECT owner_principal_id, account_uid, account_name, account_avatar
		 FROM credentials WHERE provider = ? ORDER BY id DESC LIMIT 1`,
		providerID).Scan(&o.PrincipalID, &o.Account.UID, &o.Account.Name, &o.Account.Avatar)
	if err == sql.ErrNoRows {
		return CredentialOwner{}, false, nil
	}
	if err != nil {
		return CredentialOwner{}, false, err
	}
	return o, true, nil
}

// SetCredentialOwner 绑定最新凭据行的所有者（委托关系）。
// 只在人设凭据（管理端设置/扫码完成）时由 API 层调用。
func (s *Store) SetCredentialOwner(ctx context.Context, providerID, principalID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE credentials SET owner_principal_id = ?
		 WHERE id = (SELECT id FROM credentials WHERE provider = ? ORDER BY id DESC LIMIT 1)`,
		principalID, providerID)
	return err
}

// SetCredentialAccount 写入最新凭据行的平台账号资料快照。
// 由 provider 在校验凭据（/login/status 等）成功后调用。
func (s *Store) SetCredentialAccount(ctx context.Context, providerID string, acct AccountProfile) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE credentials SET account_uid = ?, account_name = ?, account_avatar = ?
		 WHERE id = (SELECT id FROM credentials WHERE provider = ? ORDER BY id DESC LIMIT 1)`,
		acct.UID, acct.Name, acct.Avatar, providerID)
	return err
}

func (s *Store) ListCacheRows(ctx context.Context) ([]CacheRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT track_ref, file_path, size_bytes, bitrate_kbps, last_accessed_at, created_at FROM media_cache
		 ORDER BY last_accessed_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheRow
	for rows.Next() {
		var c CacheRow
		if err := rows.Scan(&c.TrackRef, &c.FilePath, &c.SizeBytes, &c.BitrateKbps, &c.LastAccessedAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
