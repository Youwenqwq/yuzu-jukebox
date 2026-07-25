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

	rooms map[string]*Room
}

func NewManager(st *store.Store, authm *auth.Manager, c *cache.Cache, reg *provider.Registry) *Manager {
	return &Manager{st: st, authm: authm, cache: c, reg: reg, rooms: map[string]*Room{}}
}

// Load 从 DB 加载全部房间并启动 actor。
func (m *Manager) Load(ctx context.Context) error {
	rows, err := m.st.ListRooms(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		m.spawn(ctx, row)
	}
	return nil
}

// Spawn 新建房间（REST 管理接口调用）。
func (m *Manager) Spawn(ctx context.Context, row store.Room) *Room {
	return m.spawn(ctx, row)
}

func (m *Manager) spawn(ctx context.Context, row store.Room) *Room {
	r := New(row.ID, row.Name, row.PasswordHash, m.st, m.authm, m.cache, m.reg)
	m.rooms[row.ID] = r
	go r.Run(ctx)
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
