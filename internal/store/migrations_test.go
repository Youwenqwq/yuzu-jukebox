package store

import (
	"context"
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

func TestMigration0032BoundsHistoricalAccelerationRetries(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "legacy-retries.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	createDistributionTestAcceleration(t, st)
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(st.db, "migrations", 31); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO distribution_requests
		(acceleration_id,track_ref,requested_at,updated_at,attempts,next_attempt_at,last_error)
		VALUES ('edgeone','ncm:exhausted',1,2,100,9999999999999,'legacy failure'),
		('edgeone','ncm:retry',1,2,2,9999999999999,'legacy failure'),
		('edgeone','ncm:after-success',1,2,100,9999999999999,'legacy failure')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO distribution_attempts
		(lease_id,acceleration_id,track_ref,owner,phase,status,started_at,updated_at,finished_at)
		VALUES ('success','edgeone','ncm:after-success','publisher','completing','succeeded',1,10,10),
		('recent','edgeone','ncm:after-success','publisher','claimed','failed',20,21,21)`); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(st.db, "migrations"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UnixMilli()
	exhausted, err := st.GetDistributionRequest(ctx, "edgeone", "ncm:exhausted", now)
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.State != "failed" || exhausted.Attempts != 100 || exhausted.NextAttemptAt != 0 {
		t.Fatalf("historical loop did not converge: %#v", exhausted)
	}
	retry, err := st.GetDistributionRequest(ctx, "edgeone", "ncm:retry", now)
	if err != nil {
		t.Fatal(err)
	}
	if retry.State != "retry_wait" || retry.NextAttemptAt > now+3_600_000 || retry.ConsecutiveAttempts != 2 {
		t.Fatalf("legacy retry was reset or left unbounded: %#v", retry)
	}
	afterSuccess, err := st.GetDistributionRequest(ctx, "edgeone", "ncm:after-success", now)
	if err != nil {
		t.Fatal(err)
	}
	if afterSuccess.State != "retry_wait" || afterSuccess.ConsecutiveAttempts != 1 || afterSuccess.Attempts != 100 {
		t.Fatalf("success audit did not reset consecutive retry budget: %#v", afterSuccess)
	}
}
