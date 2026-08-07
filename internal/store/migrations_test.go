package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
