// Package auth 实现身份、会话与出流票据。
//
// v1 只有 guest 认证：名字 + 可选全局管理员口令。
// 权限判断只认 Identity.Roles，不认登录方式，为后续 password/OIDC 留位。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Kind  string   `json:"kind"` // guest | password | oidc
	Roles []string `json:"roles"`
}

func (id Identity) HasRole(role string) bool {
	for _, r := range id.Roles {
		if r == role {
			return true
		}
	}
	return false
}

var (
	ErrSessionNotFound          = errors.New("session not found")
	ErrTicketInvalid            = errors.New("ticket invalid")
	ErrPasswordProbeRateLimited = errors.New("too many incorrect admin password attempts; try again later")
)

type session struct {
	identity  Identity
	expiresAt time.Time
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
	if st != nil {
		m.st = st
		// 恢复未过期会话；失败的行静默跳过（可能是旧格式）
		if rows, err := st.LoadSessions(context.Background(), time.Now().UnixMilli()); err == nil {
			for _, r := range rows {
				var id Identity
				if json.Unmarshal([]byte(r.IdentityJSON), &id) == nil {
					m.sessions[r.Token] = session{identity: id, expiresAt: time.UnixMilli(r.ExpiresAt)}
				}
			}
		}
		_ = st.PruneSessions(context.Background(), time.Now().UnixMilli())
	}
	return m
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
	if adminMatched {
		roles = append(roles, RoleRoomAdmin, RoleMediaAdmin)
	}
	// 访客 ID 由名字确定性派生：同名重连仍是同一人。点歌限额、
	// 移除自己点的歌等按 ID 归属的语义才能跨会话成立。
	sum := sha256.Sum256([]byte("guest:" + name))
	id := Identity{
		ID:    "g_" + hex.EncodeToString(sum[:])[:12],
		Name:  name,
		Kind:  "guest",
		Roles: roles,
	}
	token := m.IssueSession(id)
	return id, token, nil
}

// IssueSession 为一个已认证身份签发会话 token。
// 供 guest 之外的认证路径（OIDC 等）复用。
func (m *Manager) IssueSession(id Identity) string {
	token := randHex(16)
	expiresAt := time.Now().Add(m.sessionTTL)
	m.mu.Lock()
	m.sessions[token] = session{identity: id, expiresAt: expiresAt}
	m.mu.Unlock()
	if m.st != nil {
		if data, err := json.Marshal(id); err == nil {
			// 落库失败不阻断登录（内存仍有效），仅丧失重启恢复
			_ = m.st.SaveSession(context.Background(), token, string(data), expiresAt.UnixMilli())
		}
	}
	return token
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

// Session 按 token 取身份。
func (m *Manager) Session(token string) (Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[token]
	if !ok || time.Now().After(s.expiresAt) {
		return Identity{}, ErrSessionNotFound
	}
	return s.identity, nil
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
