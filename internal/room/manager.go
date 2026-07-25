package room

import (
	"context"
	"errors"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

var ErrRoomNotFound = errors.New("room not found")

// Manager 持有所有房间 actor。房间在启动时从 DB 加载，
// 生命周期与进程一致（持久房间模型）。
type Manager struct {
	st    *store.Store
	authm *auth.Manager
	cache *cache.Cache
	reg   *provider.Registry

	ctx   context.Context // actor 生命周期绑定进程，而非任何请求
	rooms map[string]*Room
}

func NewManager(ctx context.Context, st *store.Store, authm *auth.Manager, c *cache.Cache, reg *provider.Registry) *Manager {
	return &Manager{ctx: ctx, st: st, authm: authm, cache: c, reg: reg, rooms: map[string]*Room{}}
}

// Load 从 DB 加载全部房间并启动 actor。
func (m *Manager) Load() error {
	rows, err := m.st.ListRooms(m.ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		m.spawn(row)
	}
	return nil
}

// Spawn 新建房间（REST 管理接口调用）。
func (m *Manager) Spawn(row store.Room) *Room {
	return m.spawn(row)
}

func (m *Manager) spawn(row store.Room) *Room {
	r := New(row.ID, row.Name, row.PasswordHash, m.st, m.authm, m.cache, m.reg)
	m.rooms[row.ID] = r
	go r.Run(m.ctx)
	return r
}

func (m *Manager) Get(id string) (*Room, error) {
	r, ok := m.rooms[id]
	if !ok {
		return nil, ErrRoomNotFound
	}
	return r, nil
}

func (m *Manager) List() []*Room {
	out := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		out = append(out, r)
	}
	return out
}
