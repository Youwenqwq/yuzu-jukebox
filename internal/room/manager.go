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

	ctx     context.Context // actor 生命周期绑定进程，而非任何请求
	rooms   map[string]*Room
	cancels map[string]context.CancelFunc
}

func NewManager(ctx context.Context, st *store.Store, authm *auth.Manager, c *cache.Cache, reg *provider.Registry) *Manager {
	return &Manager{ctx: ctx, st: st, authm: authm, cache: c, reg: reg,
		rooms: map[string]*Room{}, cancels: map[string]context.CancelFunc{}}
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
	r := New(row.ID, row.Name, row.PasswordHash, row.PolicyJSON, m.st, m.authm, m.cache, m.reg)
	m.rooms[row.ID] = r
	rctx, cancel := context.WithCancel(m.ctx)
	m.cancels[row.ID] = cancel
	go r.Run(rctx)
	return r
}

// Delete 停止并移除房间 actor（DB 清理由调用方负责）。
func (m *Manager) Delete(id string) {
	if cancel, ok := m.cancels[id]; ok {
		cancel()
		delete(m.cancels, id)
	}
	delete(m.rooms, id)
}

func (m *Manager) Get(id string) (*Room, error) {
	r, ok := m.rooms[id]
	if !ok {
		return nil, ErrRoomNotFound
	}
	return r, nil
}

// DirectoryRoom 是 manager 汇总后的大厅目录条目。
type DirectoryRoom struct {
	ID            string
	Name          string
	PolicyRaw     string
	ListenerCount int
	NowPlaying    *NowPlayingSummary
}

// Directory 汇总各房间 actor 的实时非敏感摘要。
func (m *Manager) Directory() []DirectoryRoom {
	out := make([]DirectoryRoom, 0, len(m.rooms))
	for _, r := range m.rooms {
		snapshot, err := r.DirectorySnapshot()
		if err != nil {
			continue
		}
		out = append(out, DirectoryRoom{
			ID: r.ID, Name: r.Name, PolicyRaw: r.PolicyRaw(),
			ListenerCount: snapshot.ListenerCount, NowPlaying: snapshot.NowPlaying,
		})
	}
	return out
}

func (m *Manager) List() []*Room {
	out := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		out = append(out, r)
	}
	return out
}
