// Package auth 实现身份、会话与出流票据。
//
// v1 只有 guest 认证：名字 + 可选全局管理员口令。
// 权限判断只认 Identity.Roles，不认登录方式，为后续 password/OIDC 留位。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

const (
	RoleListener   = "listener"
	RoleRequester  = "requester"
	RoleRoomAdmin  = "room_admin"
	RoleMediaAdmin = "media_admin"
)

type Identity struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Kind                 string   `json:"kind"` // guest | password | oidc | player
	Roles                []string `json:"roles"`
	OIDCSubject          string   `json:"-"`
	IntegrationID        string   `json:"-"`
	IntegrationAdapterID string   `json:"-"`
	IntegrationScopeType string   `json:"-"`
	IntegrationScopeID   string   `json:"-"`
	IntegrationRoomID    string   `json:"-"`
	PlayerID             string   `json:"-"`
}

func (id Identity) HasRole(role string) bool {
	for _, r := range id.Roles {
		if r == role {
			return true
		}
	}
	return false
}

type IntegrationSessionSource struct {
	IntegrationID string
	AdapterID     string
	ScopeType     string
	ScopeID       string
}

var (
	ErrSessionNotFound          = errors.New("session not found")
	ErrTicketInvalid            = errors.New("ticket invalid")
	ErrPasswordProbeRateLimited = errors.New("too many incorrect admin password attempts; try again later")
)

type session struct {
	identity  Identity
	expiresAt time.Time
	source    IntegrationSessionSource
}

type ticket struct {
	trackRef   string
	identityID string
	expiresAt  time.Time
}

// Manager 管理会话与票据。会话可持久化到 store（重启不失效）；
// st 为 nil 时退化为纯内存（测试用）。
type Manager struct {
	adminPassword  string
	sessionTTL     time.Duration
	ticketTTL      time.Duration
	st             *store.Store
	passwordProbes *passwordProbeLimiter

	mu       sync.Mutex
	sessions map[string]session
	tickets  map[string]ticket
}

func NewManager(adminPassword string, st *store.Store) *Manager {
	m := &Manager{
		adminPassword:  adminPassword,
		sessionTTL:     24 * time.Hour,
		ticketTTL:      5 * time.Minute,
		sessions:       map[string]session{},
		tickets:        map[string]ticket{},
		passwordProbes: newPasswordProbeLimiter(),
	}
	if st == nil {
		return m
	}

	m.st = st
	ctx := context.Background()
	now := time.Now().UnixMilli()
	// identity_json is retained only to migrate sessions created before
	// principals became authoritative. Runtime authorization always reloads
	// the current principal below in Session.
	if rows, err := st.LoadSessions(ctx, now); err == nil {
		type legacyPrincipal struct {
			identity  Identity
			expiresAt int64
		}
		legacyPrincipals := make(map[string]legacyPrincipal)
		for _, r := range rows {
			var id Identity
			if json.Unmarshal([]byte(r.IdentityJSON), &id) != nil || id.ID == "" {
				continue
			}
			source := IntegrationSessionSource{
				IntegrationID: r.IntegrationID,
				AdapterID:     r.AdapterID,
				ScopeType:     r.ScopeType,
				ScopeID:       r.ScopeID,
			}
			m.sessions[r.Token] = session{
				identity: id, source: source, expiresAt: time.UnixMilli(r.ExpiresAt),
			}
			previous, ok := legacyPrincipals[id.ID]
			if !ok || r.ExpiresAt > previous.expiresAt {
				legacyPrincipals[id.ID] = legacyPrincipal{identity: id, expiresAt: r.ExpiresAt}
			}
		}
		for _, legacy := range legacyPrincipals {
			if _, err := st.GetPrincipal(ctx, legacy.identity.ID); errors.Is(err, sql.ErrNoRows) {
				_ = st.UpsertPrincipal(ctx, principalFromIdentity(legacy.identity, true))
			}
		}
	}
	_ = st.PruneSessions(ctx, now)
	return m
}

func principalFromIdentity(id Identity, active bool) store.Principal {
	return store.Principal{
		ID:          id.ID,
		Name:        id.Name,
		Kind:        id.Kind,
		OIDCSubject: id.OIDCSubject,
		Roles:       id.Roles,
		Active:      active,
	}
}

func identityFromPrincipal(p store.Principal) Identity {
	return Identity{
		ID:          p.ID,
		Name:        p.Name,
		Kind:        p.Kind,
		Roles:       p.Roles,
		OIDCSubject: p.OIDCSubject,
	}
}

// GuestAuth 访客认证。adminPassword 命中全局管理员口令时授予管理角色。
func (m *Manager) GuestAuth(name, adminPassword, remoteAddr string) (Identity, string, error) {
	adminMatched := m.adminPassword != "" && adminPassword == m.adminPassword
	if !m.passwordProbes.allow(remoteAddr, adminPassword != "", adminMatched) {
		return Identity{}, "", ErrPasswordProbeRateLimited
	}
	if name == "" {
		return Identity{}, "", errors.New("name required")
	}
	roles := []string{RoleListener, RoleRequester}
	kind := "guest"
	idPrefix := "g_"
	hashInput := "guest:" + name
	if adminMatched {
		roles = append(roles, RoleRoomAdmin, RoleMediaAdmin)
		// A shared display name is not authentication. Password-authenticated
		// administrators therefore use a distinct principal namespace so a
		// pre-existing ordinary guest session can never inherit the grant.
		kind = "password"
		idPrefix = "p_"
		hashInput = "password:" + name
	}
	sum := sha256.Sum256([]byte(hashInput))
	id := Identity{
		ID:    idPrefix + hex.EncodeToString(sum[:])[:12],
		Name:  name,
		Kind:  kind,
		Roles: roles,
	}
	token, err := m.IssueAuthenticatedSession(id)
	if err != nil {
		return Identity{}, "", err
	}
	return id, token, nil
}

// IssueAuthenticatedSession persists the freshly authenticated identity as the
// current principal and issues a normal 24-hour session.
func (m *Manager) IssueAuthenticatedSession(id Identity) (string, error) {
	if err := m.persistPrincipal(id); err != nil {
		return "", err
	}
	token, _, err := m.issueSession(id, m.sessionTTL, IntegrationSessionSource{})
	return token, err
}

// IssueSession preserves the existing convenience API for trusted in-process
// callers and tests. Authentication handlers should use IssueAuthenticatedSession
// so persistence failures are observable.
func (m *Manager) IssueSession(id Identity) string {
	token, err := m.IssueAuthenticatedSession(id)
	if err != nil {
		return ""
	}
	return token
}

// IssueSessionWithTTL issues a short-lived session for an existing principal.
// It never writes authorization state supplied by the caller.
func (m *Manager) IssueSessionWithTTL(id Identity, ttl time.Duration) (string, int64, error) {
	id, err := m.currentIdentity(id)
	if err != nil {
		return "", 0, err
	}
	return m.issueSession(id, ttl, IntegrationSessionSource{})
}

// IssueIntegrationSession issues a short-lived actor session tied to its
// originating trusted Integration scope. Disabling, deleting or rotating that
// Integration can revoke only these sessions without affecting the Principal's
// own login.
func (m *Manager) IssueIntegrationSession(
	id Identity,
	source IntegrationSessionSource,
	ttl time.Duration,
) (string, int64, error) {
	id, err := m.currentIdentity(id)
	if err != nil {
		return "", 0, err
	}
	if m.st != nil {
		integration, err := m.st.GetIntegration(context.Background(), source.IntegrationID)
		if err != nil || !integration.Active {
			return "", 0, ErrSessionNotFound
		}
	}
	return m.issueSession(id, ttl, source)
}

func (m *Manager) currentIdentity(id Identity) (Identity, error) {
	if m.st == nil {
		return id, nil
	}
	principal, err := m.st.GetPrincipal(context.Background(), id.ID)
	if err != nil {
		return Identity{}, err
	}
	if !principal.Active {
		return Identity{}, ErrSessionNotFound
	}
	return identityFromPrincipal(principal), nil
}

func (m *Manager) persistPrincipal(id Identity) error {
	if m.st == nil {
		return nil
	}
	if current, err := m.st.GetPrincipal(context.Background(), id.ID); err == nil {
		if !current.Active {
			return ErrSessionNotFound
		}
		return m.st.UpsertPrincipal(context.Background(), principalFromIdentity(id, true))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return m.st.UpsertPrincipal(context.Background(), principalFromIdentity(id, true))
}

func (m *Manager) issueSession(
	id Identity,
	ttl time.Duration,
	source IntegrationSessionSource,
) (string, int64, error) {
	id.IntegrationID = source.IntegrationID
	id.IntegrationAdapterID = source.AdapterID
	id.IntegrationScopeType = source.ScopeType
	id.IntegrationScopeID = source.ScopeID
	token := randHex(16)
	expiresAt := time.Now().Add(ttl).UTC().Truncate(time.Millisecond)
	expiresAtMS := expiresAt.UnixMilli()
	if m.st != nil {
		data, err := json.Marshal(id)
		if err != nil {
			return "", 0, err
		}
		if err := m.st.SaveSessionWithActorSource(
			context.Background(),
			token,
			string(data),
			store.SessionSource{
				IntegrationID: source.IntegrationID,
				AdapterID:     source.AdapterID,
				ScopeType:     source.ScopeType,
				ScopeID:       source.ScopeID,
			},
			expiresAtMS,
		); err != nil {
			return "", 0, err
		}
	}
	m.mu.Lock()
	m.sessions[token] = session{identity: id, source: source, expiresAt: expiresAt}
	m.mu.Unlock()
	return token, expiresAtMS, nil
}

// Revoke 吊销会话（logout）。幂等。
func (m *Manager) Revoke(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
	if m.st != nil {
		_ = m.st.DeleteSession(context.Background(), token)
	}
}

// RevokeIntegration invalidates every actor session issued through one
// Integration without touching the same Principals' WebUI or CLI sessions.
func (m *Manager) RevokeIntegration(integrationID string) {
	m.mu.Lock()
	for token, current := range m.sessions {
		if current.source.IntegrationID == integrationID {
			delete(m.sessions, token)
		}
	}
	m.mu.Unlock()
	if m.st != nil {
		_ = m.st.DeleteSessionsByIntegration(context.Background(), integrationID)
	}
}

// PruneExpired removes stale sessions from memory and persistent storage.
func (m *Manager) PruneExpired(ctx context.Context, now time.Time) error {
	m.mu.Lock()
	for token, current := range m.sessions {
		if !now.Before(current.expiresAt) {
			delete(m.sessions, token)
		}
	}
	for token, current := range m.tickets {
		if !now.Before(current.expiresAt) {
			delete(m.tickets, token)
		}
	}
	m.mu.Unlock()
	if m.st == nil {
		return nil
	}
	return m.st.PruneSessions(ctx, now.UnixMilli())
}

// Session 按 token 取身份。
func (m *Manager) Session(token string) (Identity, error) {
	m.mu.Lock()
	s, ok := m.sessions[token]
	if ok && time.Now().After(s.expiresAt) {
		delete(m.sessions, token)
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		return Identity{}, ErrSessionNotFound
	}
	if m.st == nil {
		return s.identity, nil
	}

	principal, err := m.st.GetPrincipal(context.Background(), s.identity.ID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !principal.Active) {
		return Identity{}, ErrSessionNotFound
	}
	if err != nil {
		return Identity{}, err
	}
	if s.source.IntegrationID != "" {
		integration, err := m.st.GetIntegration(context.Background(), s.source.IntegrationID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !integration.Active) {
			return Identity{}, ErrSessionNotFound
		}
		if err != nil {
			return Identity{}, err
		}
	}
	id := identityFromPrincipal(principal)
	id.IntegrationID = s.source.IntegrationID
	id.IntegrationAdapterID = s.source.AdapterID
	id.IntegrationScopeType = s.source.ScopeType
	id.IntegrationScopeID = s.source.ScopeID
	if s.source.IntegrationID != "" && s.source.AdapterID != "" &&
		s.source.ScopeType != "" && s.source.ScopeID != "" {
		roomID, err := m.st.ResolveExternalScopeRoom(
			context.Background(),
			s.source.IntegrationID,
			s.source.AdapterID,
			s.source.ScopeType,
			s.source.ScopeID,
		)
		if err == nil {
			id.IntegrationRoomID = roomID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Identity{}, err
		}
	}
	return id, nil
}

// IssueTicket 签发某身份拉取某曲目的出流票据。
// 票据在 TTL 内可复用——客户端的 Range 请求与断线重试都依赖同一 URL。
func (m *Manager) IssueTicket(identityID, trackRef string) string {
	token := randHex(16)
	m.mu.Lock()
	m.tickets[token] = ticket{
		trackRef:   trackRef,
		identityID: identityID,
		expiresAt:  time.Now().Add(m.ticketTTL),
	}
	m.mu.Unlock()
	return token
}

// ValidateTicket 校验票据与曲目匹配。
func (m *Manager) ValidateTicket(token, trackRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickets[token]
	if !ok || time.Now().After(t.expiresAt) || t.trackRef != trackRef {
		return ErrTicketInvalid
	}
	return nil
}

// RevokeTrack 使某曲目的所有票据失效（曲目播完时调用）。
func (m *Manager) RevokeTrack(trackRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, t := range m.tickets {
		if t.trackRef == trackRef {
			delete(m.tickets, k)
		}
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
