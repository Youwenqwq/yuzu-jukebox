package auth

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestSessionUsesCurrentPrincipalState(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	manager := NewManager("", st)
	token := manager.IssueSession(Identity{
		ID: "principal-1", Name: "Before", Kind: "oidc",
		Roles: []string{RoleListener, RoleRequester},
	})
	ctx := context.Background()
	principal, err := st.GetPrincipal(ctx, "principal-1")
	if err != nil {
		t.Fatal(err)
	}
	principal.Name = "After"
	principal.Roles = []string{RoleListener, RoleRoomAdmin}
	if err := st.UpsertPrincipal(ctx, principal); err != nil {
		t.Fatal(err)
	}

	identity, err := manager.Session(token)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Name != "After" || !identity.HasRole(RoleRoomAdmin) || identity.HasRole(RoleRequester) {
		t.Fatalf("session identity did not observe current principal: %#v", identity)
	}

	principal.Active = false
	if err := st.UpsertPrincipal(ctx, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Session(token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("disabled principal session error = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionTokenStoredAsDigestAndLifecycleUsesRawToken(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "session-token.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	manager := NewManager("", st)
	token := manager.IssueSession(Identity{
		ID: "hashed-session", Name: "Hashed Session", Kind: "guest",
		Roles: []string{RoleListener, RoleRequester},
	})
	if token == "" {
		t.Fatal("IssueSession returned an empty raw token")
	}
	digest := sessionTokenDigest(token)
	if _, ok := manager.sessions[digest]; !ok {
		t.Fatal("in-memory session is not keyed by the token digest")
	}
	if _, ok := manager.sessions[token]; ok {
		t.Fatal("raw token was retained as an in-memory session key")
	}

	var persistedToken string
	if err := st.DB().QueryRowContext(ctx, `SELECT token FROM sessions`).Scan(&persistedToken); err != nil {
		t.Fatal(err)
	}
	if persistedToken != digest {
		t.Fatalf("persisted session token = %q, want digest %q", persistedToken, digest)
	}
	if persistedToken == token {
		t.Fatal("raw session token was persisted")
	}
	if _, err := manager.Session(token); err != nil {
		t.Fatalf("fresh raw-token lookup failed: %v", err)
	}
	if _, err := manager.Session(digest); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("stored digest authenticated as a bearer token: %v", err)
	}

	reloaded := NewManager("", st)
	if _, err := reloaded.Session(token); err != nil {
		t.Fatalf("raw-token lookup after manager reload failed: %v", err)
	}
	reloaded.Revoke(token)
	if _, err := reloaded.Session(token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("revoked session error = %v, want ErrSessionNotFound", err)
	}
	var persistedCount int
	if err := st.DB().QueryRowContext(
		ctx, `SELECT COUNT(*) FROM sessions WHERE token = ?`, digest,
	).Scan(&persistedCount); err != nil {
		t.Fatal(err)
	}
	if persistedCount != 0 {
		t.Fatalf("persisted session rows after revoke = %d, want 0", persistedCount)
	}
}

func TestExistingSessionMigratesPrincipal(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "migration.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	legacy := Identity{
		ID: "legacy-principal", Name: "Legacy", Kind: "guest",
		Roles: []string{RoleListener, RoleRequester},
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession(
		context.Background(), sessionTokenDigest("legacy-token"), string(payload), time.Now().Add(time.Hour).UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}

	manager := NewManager("", st)
	identity, err := manager.Session("legacy-token")
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != legacy.ID || !identity.HasRole(RoleRequester) {
		t.Fatalf("restored identity = %#v, want %#v", identity, legacy)
	}
	principal, err := st.GetPrincipal(context.Background(), legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Name != legacy.Name || !principal.Active || len(principal.Roles) != len(legacy.Roles) {
		t.Fatalf("migrated principal = %#v", principal)
	}
	principal.Name = "Current"
	principal.Roles = []string{RoleListener, RoleRoomAdmin}
	if err := st.UpsertPrincipal(context.Background(), principal); err != nil {
		t.Fatal(err)
	}
	restoredManager := NewManager("", st)
	identity, err = restoredManager.Session("legacy-token")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Name != "Current" || !identity.HasRole(RoleRoomAdmin) || identity.HasRole(RoleRequester) {
		t.Fatalf("session restore used stale identity payload: %#v", identity)
	}
}

func TestOIDCAndGuestSessionsPersistQueryablePrincipals(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "login.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	manager := NewManager("administrator-password", st)

	oidcIdentity := OIDCIdentity(
		OIDCClaims{Sub: "stable-subject", Username: "oidc-user"},
		[]string{RoleListener, RoleRequester},
	)
	manager.IssueSession(oidcIdentity)
	principal, err := st.GetPrincipalByOIDCSubject(context.Background(), "stable-subject")
	if err != nil {
		t.Fatal(err)
	}
	if principal.ID != oidcIdentity.ID || principal.Name != oidcIdentity.Name {
		t.Fatalf("OIDC principal = %#v, identity = %#v", principal, oidcIdentity)
	}

	guestIdentity, _, err := manager.GuestAuth("guest-user", "administrator-password", "192.0.2.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	principal, err = st.GetPrincipal(context.Background(), guestIdentity.ID)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Name != guestIdentity.Name || !principal.Active || !containsRole(principal.Roles, RoleRoomAdmin) {
		t.Fatalf("guest principal = %#v", principal)
	}
}

func TestIssueSessionWithTTLReturnsExactShortExpiry(t *testing.T) {
	manager := NewManager("", nil)
	identity := Identity{
		ID: "short-lived", Name: "Short Lived", Kind: "guest",
		Roles: []string{RoleListener, RoleRequester},
	}
	before := time.Now().UnixMilli()
	token, expiresAt, err := manager.IssueSessionWithTTL(identity, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("IssueSessionWithTTL returned an empty token")
	}
	if expiresAt < before+90 || expiresAt > time.Now().UnixMilli()+110 {
		t.Fatalf("expires_at = %d, want about 100ms from issuance", expiresAt)
	}
	stored := manager.sessions[sessionTokenDigest(token)]
	if stored.expiresAt.UnixMilli() != expiresAt {
		t.Fatalf("stored expiry = %d, returned expiry = %d", stored.expiresAt.UnixMilli(), expiresAt)
	}

	normalToken := manager.IssueSession(identity)
	normalTTL := time.Until(manager.sessions[sessionTokenDigest(normalToken)].expiresAt)
	if normalTTL < 30*24*time.Hour-time.Minute || normalTTL > 30*24*time.Hour {
		t.Fatalf("normal session TTL = %v, want 30 days", normalTTL)
	}

	time.Sleep(200 * time.Millisecond)
	if _, err := manager.Session(token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired short session error = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionSlidesNormalExpiry(t *testing.T) {
	manager := NewManager("", nil, WithSessionTTL(2*time.Hour))
	token := manager.IssueSession(Identity{
		ID: "sliding-session", Name: "Sliding Session", Kind: "guest",
		Roles: []string{RoleListener, RoleRequester},
	})
	digest := sessionTokenDigest(token)
	current := manager.sessions[digest]
	current.expiresAt = time.Now().Add(time.Minute)
	previousExpiry := current.expiresAt
	manager.sessions[digest] = current

	if _, err := manager.Session(token); err != nil {
		t.Fatal(err)
	}
	renewedExpiry := manager.sessions[digest].expiresAt
	if !renewedExpiry.After(previousExpiry.Add(time.Hour)) {
		t.Fatalf("renewed expiry = %v, previous expiry = %v", renewedExpiry, previousExpiry)
	}
}

func TestSessionExpiryPersistenceIsThrottled(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "sliding-throttle.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	manager := NewManager("", st, WithSessionTTL(2*time.Hour))
	token := manager.IssueSession(Identity{
		ID: "throttled-session", Name: "Throttled Session", Kind: "guest",
		Roles: []string{RoleListener, RoleRequester},
	})
	digest := sessionTokenDigest(token)
	var persistedBefore int64
	if err := st.DB().QueryRowContext(
		ctx, `SELECT expires_at FROM sessions WHERE token = ?`, digest,
	).Scan(&persistedBefore); err != nil {
		t.Fatal(err)
	}

	current := manager.sessions[digest]
	current.expiresAt = time.Now().Add(time.Minute)
	manager.sessions[digest] = current
	if _, err := manager.Session(token); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Session(token); err != nil {
		t.Fatal(err)
	}

	var persistedAfter int64
	if err := st.DB().QueryRowContext(
		ctx, `SELECT expires_at FROM sessions WHERE token = ?`, digest,
	).Scan(&persistedAfter); err != nil {
		t.Fatal(err)
	}
	if persistedAfter != persistedBefore {
		t.Fatalf("persisted expiry changed below throttle threshold: before=%d after=%d", persistedBefore, persistedAfter)
	}
}

func TestSessionPersistsExpiryBeyondThrottleThreshold(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "sliding-persist.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	manager := NewManager("", st)
	token := manager.IssueSession(Identity{
		ID: "persisted-sliding-session", Name: "Persisted Sliding Session", Kind: "guest",
		Roles: []string{RoleListener, RoleRequester},
	})
	digest := sessionTokenDigest(token)
	current := manager.sessions[digest]
	stalePersistedExpiry := current.persistedExpiresAt.Add(-25 * time.Hour)
	if err := st.UpdateSessionExpiry(ctx, digest, stalePersistedExpiry.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	const persistedIdentity = `{"id":"persistence-sentinel"}`
	if _, err := st.DB().ExecContext(
		ctx, `UPDATE sessions SET identity_json = ? WHERE token = ?`, persistedIdentity, digest,
	); err != nil {
		t.Fatal(err)
	}
	current.persistedExpiresAt = stalePersistedExpiry
	manager.sessions[digest] = current

	if _, err := manager.Session(token); err != nil {
		t.Fatal(err)
	}
	var persistedAfter int64
	var identityAfter string
	if err := st.DB().QueryRowContext(
		ctx, `SELECT expires_at, identity_json FROM sessions WHERE token = ?`, digest,
	).Scan(&persistedAfter, &identityAfter); err != nil {
		t.Fatal(err)
	}
	if persistedAfter <= stalePersistedExpiry.UnixMilli() {
		t.Fatalf("persisted expiry = %d, want later than %d", persistedAfter, stalePersistedExpiry.UnixMilli())
	}
	if persistedAfter != manager.sessions[digest].expiresAt.UnixMilli() {
		t.Fatalf("persisted expiry = %d, in-memory expiry = %d", persistedAfter, manager.sessions[digest].expiresAt.UnixMilli())
	}
	if identityAfter != persistedIdentity {
		t.Fatalf("expiry update rewrote identity_json: got %q, want %q", identityAfter, persistedIdentity)
	}
}

func TestExpiredSessionIsNotRenewed(t *testing.T) {
	manager := NewManager("", nil, WithSessionTTL(2*time.Hour))
	token := manager.IssueSession(Identity{
		ID: "expired-session", Name: "Expired Session", Kind: "guest",
		Roles: []string{RoleListener, RoleRequester},
	})
	digest := sessionTokenDigest(token)
	current := manager.sessions[digest]
	current.expiresAt = time.Now().Add(-time.Millisecond)
	manager.sessions[digest] = current

	if _, err := manager.Session(token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired session error = %v, want ErrSessionNotFound", err)
	}
	if _, ok := manager.sessions[digest]; ok {
		t.Fatal("expired session remained in memory")
	}
}

func TestIntegrationSessionDoesNotSlide(t *testing.T) {
	manager := NewManager("", nil, WithSessionTTL(2*time.Hour))
	token, issuedExpiry, err := manager.IssueIntegrationSession(
		Identity{
			ID: "integration-actor", Name: "Integration Actor", Kind: "guest",
			Roles: []string{RoleListener, RoleRequester},
		},
		IntegrationSessionSource{IntegrationID: "bridge"},
		5*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	digest := sessionTokenDigest(token)
	if _, err := manager.Session(token); err != nil {
		t.Fatal(err)
	}
	if got := manager.sessions[digest].expiresAt.UnixMilli(); got != issuedExpiry {
		t.Fatalf("Integration actor expiry slid from %d to %d", issuedExpiry, got)
	}
}

func TestPasswordAdminDoesNotElevateSameNameGuest(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "admin-isolation.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	manager := NewManager("administrator-password", st)
	guest, guestToken, err := manager.GuestAuth("shared-name", "", "192.0.2.1:1000")
	if err != nil {
		t.Fatal(err)
	}
	admin, _, err := manager.GuestAuth("shared-name", "administrator-password", "192.0.2.1:1001")
	if err != nil {
		t.Fatal(err)
	}
	if guest.ID == admin.ID || guest.Kind != "guest" || admin.Kind != "password" {
		t.Fatalf("guest = %#v, admin = %#v", guest, admin)
	}
	currentGuest, err := manager.Session(guestToken)
	if err != nil {
		t.Fatal(err)
	}
	if currentGuest.HasRole(RoleRoomAdmin) || currentGuest.HasRole(RoleMediaAdmin) {
		t.Fatalf("same-name guest inherited administrator roles: %#v", currentGuest)
	}
}

func TestLegacyPrincipalUsesLatestSessionSnapshot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "legacy-order.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	oldIdentity := Identity{
		ID: "legacy-latest", Name: "Legacy", Kind: "oidc",
		Roles: []string{RoleListener, RoleRequester, RoleRoomAdmin},
	}
	currentIdentity := oldIdentity
	currentIdentity.Roles = []string{RoleListener, RoleRequester}
	oldPayload, _ := json.Marshal(oldIdentity)
	currentPayload, _ := json.Marshal(currentIdentity)
	now := time.Now()
	if err := st.SaveSession(context.Background(), sessionTokenDigest("old-token"), string(oldPayload), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession(context.Background(), sessionTokenDigest("current-token"), string(currentPayload), now.Add(2*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	manager := NewManager("", st)
	identity, err := manager.Session("old-token")
	if err != nil {
		t.Fatal(err)
	}
	if identity.HasRole(RoleRoomAdmin) {
		t.Fatalf("latest legacy snapshot did not revoke administrator role: %#v", identity)
	}
}

func containsRole(roles []string, want string) bool {
	for _, role := range roles {
		if role == want {
			return true
		}
	}
	return false
}
