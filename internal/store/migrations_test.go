package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestMigration0028HashesExistingSessionTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-session.db")
	legacyDB, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	legacyDB.SetMaxOpenConns(1)

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		_ = legacyDB.Close()
		t.Fatal(err)
	}
	if err := goose.UpTo(legacyDB, "migrations", 27); err != nil {
		_ = legacyDB.Close()
		t.Fatal(err)
	}

	const rawToken = "legacy-raw-session-token"
	const identityJSON = `{"id":"legacy-principal","name":"Legacy","kind":"guest","roles":["listener"]}`
	expiresAt := time.Now().Add(time.Hour).UnixMilli()
	if _, err := legacyDB.Exec(
		`INSERT INTO sessions (token, identity_json, expires_at) VALUES (?, ?, ?)`,
		rawToken, identityJSON, expiresAt,
	); err != nil {
		_ = legacyDB.Close()
		t.Fatal(err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	digest := sha256.Sum256([]byte(rawToken))
	wantToken := hex.EncodeToString(digest[:])
	var gotToken, gotIdentityJSON string
	var gotExpiresAt int64
	if err := st.DB().QueryRow(
		`SELECT token, identity_json, expires_at FROM sessions`,
	).Scan(&gotToken, &gotIdentityJSON, &gotExpiresAt); err != nil {
		t.Fatal(err)
	}
	if gotToken != wantToken {
		t.Fatalf("migrated token = %q, want SHA-256 digest %q", gotToken, wantToken)
	}
	if gotToken == rawToken {
		t.Fatal("migration left the raw session token at rest")
	}
	if gotIdentityJSON != identityJSON || gotExpiresAt != expiresAt {
		t.Fatalf("migration changed session data: identity_json=%q expires_at=%d", gotIdentityJSON, gotExpiresAt)
	}
}

func TestMigration0033FreshSchemaHasNoAcceleration(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "fresh.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	assertNoAccelerationSchema(t, st.DB())
}

func TestMigration0033RemovesAccelerationAndPreservesUnrelatedState(t *testing.T) {
	for _, version := range []int64{31, 32} {
		t.Run(fmt.Sprintf("from_%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			db.SetMaxOpenConns(1)
			goose.SetBaseFS(migrationsFS)
			if err := goose.SetDialect("sqlite3"); err != nil {
				t.Fatal(err)
			}
			if err := goose.UpTo(db, "migrations", version); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`
				INSERT INTO accelerations (id, name, kind, backend_token, created_at, updated_at)
				VALUES ('edge', 'Edge', 'edgeone', 'retired-secret', 1, 1);
				INSERT INTO distribution_requests (acceleration_id, track_ref, requested_at, updated_at, attempts)
				VALUES ('edge', 'ncm:cached', 1, 1, 100);
				INSERT INTO distribution_leases (id, acceleration_id, track_ref, owner, expires_at, created_at)
				VALUES ('lease', 'edge', 'ncm:cached', 'publisher', 100, 1);
				INSERT INTO distribution_candidates
					(acceleration_id, track_ref, content_version, locator, size_bytes, content_type, created_at, updated_at)
				VALUES ('edge', 'ncm:cached', 'v1', 'object', 10, 'audio/mpeg', 1, 1);
				INSERT INTO distribution_publishers (acceleration_id, owner, last_seen_at)
				VALUES ('edge', 'publisher', 1);
				INSERT INTO distribution_attempts
					(lease_id, acceleration_id, track_ref, owner, started_at, updated_at)
				VALUES ('lease', 'edge', 'ncm:cached', 'publisher', 1, 1);
				INSERT INTO distribution_metrics (acceleration_id, name, value, updated_at)
				VALUES ('edge', 'published', 1, 1);
				INSERT INTO distribution_metric_buckets (acceleration_id, bucket_start, name, value)
				VALUES ('edge', 1, 'published', 1);
				INSERT INTO acceleration_objects
					(acceleration_id, locator, size_bytes, last_accessed_at, created_at, updated_at)
				VALUES ('edge', 'object', 10, 1, 1, 1);
				INSERT INTO acceleration_storage_reservations
					(lease_id, acceleration_id, locator, size_bytes, expires_at, created_at)
				VALUES ('lease', 'edge', 'object', 10, 100, 1);
				INSERT INTO acceleration_deletion_jobs (id, acceleration_id, locator, created_at, updated_at)
				VALUES ('deletion', 'edge', 'object', 1, 1);
				INSERT INTO acceleration_inventory_snapshots (id, acceleration_id, owner, observed_at, created_at)
				VALUES ('snapshot', 'edge', 'publisher', 1, 1);
				INSERT INTO acceleration_inventory_objects (snapshot_id, acceleration_id, locator, size_bytes)
				VALUES ('snapshot', 'edge', 'object', 10);
				INSERT INTO acceleration_inventory_scans (id, acceleration_id, requested_at, updated_at)
				VALUES ('scan', 'edge', 1, 1);
				INSERT INTO acceleration_storage_status (acceleration_id, observed_bytes)
				VALUES ('edge', 10);
				CREATE TRIGGER acceleration_audit AFTER INSERT ON accelerations
				BEGIN
					INSERT INTO audit_log (actor_id, action, created_at) VALUES ('edge', 'created', 1);
				END;

				INSERT INTO users (id, kind, name, roles_json, created_at)
				VALUES ('admin', 'password', 'Admin', '["listener","sys_admin","media_admin","sys_admin"]', 1),
				       ('exclusive', 'oidc', 'Exclusive', '["sys_admin"]', 1),
				       ('ordinary', 'guest', 'Ordinary', '[ "listener" ]', 1),
				       ('invalid', 'guest', 'Invalid', 'legacy-invalid-json', 1);
				INSERT INTO sessions (token, identity_json, expires_at)
				VALUES ('session', '{"id":"admin","name":"Admin","kind":"password","roles":["sys_admin","listener"],"avatar":"keep"}', 100),
				       ('exclusive', '{"id":"exclusive","roles":["sys_admin"]}', 100),
				       ('ordinary', '{ "id": "ordinary", "roles": ["listener"] }', 100),
				       ('invalid', 'legacy-invalid-json', 100);
				INSERT INTO rooms (id, name, trusted_roles_json, policy_json, created_at)
				VALUES ('room', 'Room', '["sys_admin","room_admin"]', '{"allow_guest":true}', 1),
				       ('exclusive', 'Exclusive', '["sys_admin"]', '{}', 1),
				       ('ordinary', 'Ordinary', '[ "listener" ]', '{}', 1),
				       ('invalid', 'Invalid', 'legacy-invalid-json', '{}', 1);
				INSERT INTO media_cache (track_ref, file_path, size_bytes, last_accessed_at, created_at)
				VALUES ('ncm:cached', '/cache/music.mp3', 10, 2, 1);
				INSERT INTO credentials (provider, payload, status) VALUES ('ncm', X'010203', 'ok');
				INSERT INTO room_queue (room_id, ord, entry_id, track_ref, added_at)
				VALUES ('room', 0, 'entry', 'ncm:cached', 1);
			`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			st, err := Open(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			assertNoAccelerationSchema(t, st.DB())
			for query, want := range map[string]string{
				`SELECT roles_json FROM users WHERE id = 'admin'`:                                                                                       `["listener","media_admin"]`,
				`SELECT roles_json FROM users WHERE id = 'exclusive'`:                                                                                   `[]`,
				`SELECT roles_json FROM users WHERE id = 'ordinary'`:                                                                                    `[ "listener" ]`,
				`SELECT roles_json FROM users WHERE id = 'invalid'`:                                                                                     `legacy-invalid-json`,
				`SELECT json_extract(identity_json, '$.roles') FROM sessions WHERE token = 'session'`:                                                   `["listener"]`,
				`SELECT json_remove(identity_json, '$.roles') FROM sessions WHERE token = 'session'`:                                                    `{"id":"admin","name":"Admin","kind":"password","avatar":"keep"}`,
				`SELECT json_extract(identity_json, '$.roles') FROM sessions WHERE token = 'exclusive'`:                                                 `[]`,
				`SELECT identity_json FROM sessions WHERE token = 'ordinary'`:                                                                           `{ "id": "ordinary", "roles": ["listener"] }`,
				`SELECT identity_json FROM sessions WHERE token = 'invalid'`:                                                                            `legacy-invalid-json`,
				`SELECT CAST(expires_at AS TEXT) FROM sessions WHERE token = 'session'`:                                                                 `100`,
				`SELECT trusted_roles_json FROM rooms WHERE id = 'room'`:                                                                                `["room_admin"]`,
				`SELECT trusted_roles_json FROM rooms WHERE id = 'exclusive'`:                                                                           `[]`,
				`SELECT trusted_roles_json FROM rooms WHERE id = 'ordinary'`:                                                                            `[ "listener" ]`,
				`SELECT trusted_roles_json FROM rooms WHERE id = 'invalid'`:                                                                             `legacy-invalid-json`,
				`SELECT policy_json FROM rooms WHERE id = 'room'`:                                                                                       `{"allow_guest":true}`,
				`SELECT file_path || ':' || size_bytes || ':' || last_accessed_at || ':' || created_at FROM media_cache WHERE track_ref = 'ncm:cached'`: `/cache/music.mp3:10:2:1`,
				`SELECT provider || ':' || hex(payload) || ':' || status FROM credentials`:                                                              `ncm:010203:ok`,
				`SELECT entry_id || ':' || track_ref FROM room_queue WHERE room_id = 'room'`:                                                            `entry:ncm:cached`,
			} {
				var got string
				if err := st.DB().QueryRow(query).Scan(&got); err != nil {
					t.Fatalf("%s: %v", query, err)
				}
				if got != want {
					t.Fatalf("%s = %q, want %q", query, got, want)
				}
			}
		})
	}
}

func assertNoAccelerationSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE name GLOB '*acceleration*' OR name GLOB '*distribution*'
		   OR tbl_name GLOB '*acceleration*' OR tbl_name GLOB '*distribution*'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("retained %d acceleration schema resources", count)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("migration left broken foreign keys")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
