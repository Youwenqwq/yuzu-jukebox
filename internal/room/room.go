// Package room 实现房间 actor：每个房间一个 goroutine，
// 所有操作经 inbound channel 串行处理，零锁。
//
// 播放状态是纯函数模型：PositionMs + UpdatedAt + Playing + Rate，
// 任何观察者据此与服务器时钟推算一致的位置；系统内不存在"当前进度"字段。
package room

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"

	"golang.org/x/crypto/bcrypt"
)

// ClientConn 是房间对 WS 客户端的抽象（由 wsapi 实现，避免包环依赖）。
type ClientConn interface {
	ID() string
	Identity() auth.Identity
	// Send 非阻塞；客户端发送缓冲满时由实现方断开。
	Send(msg any)
}

// QueueEntry 队列条目。StreamURL 仅在下发给具体客户端时填充（按身份签发票据）。
type QueueEntry struct {
	EntryID     string `json:"entry_id"`
	TrackRef    string `json:"track_ref"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	DurationMs  int64  `json:"duration_ms"`
	RequestedBy string `json:"requested_by"`
	AddedAt     int64  `json:"added_at"`
	StreamURL   string `json:"stream_url,omitempty"`
}

// Playback 播放状态五元组（权威）。
type Playback struct {
	Current    *QueueEntry `json:"current"`
	PositionMs int64       `json:"position_ms"`
	UpdatedAt  int64       `json:"updated_at"`
	Playing    bool        `json:"playing"`
	Rate       float64     `json:"rate"`
}

type actKind int

const (
	actJoin actKind = iota
	actLeave
	actAdd
	actRemove
	actMove
	actPause
	actResume
	actSeek
	actSkip
	actTimerEnd
)

type action struct {
	kind    actKind
	client  ClientConn
	actor   auth.Identity
	entry   QueueEntry
	entryID string
	toIndex int
	posMs   int64
	timerID uint64
	result  chan error
}

var (
	ErrQueueEmpty    = errors.New("queue is empty")
	ErrEntryNotFound = errors.New("queue entry not found")
	ErrNoPlayback    = errors.New("nothing is playing")
	ErrForbidden     = errors.New("forbidden")
)

type Room struct {
	ID           string
	Name         string
	passwordHash string

	st    *store.Store
	authm *auth.Manager
	cache *cache.Cache
	reg   *provider.Registry

	inbound chan action
}

func New(id, name, passwordHash string, st *store.Store, authm *auth.Manager, c *cache.Cache, reg *provider.Registry) *Room {
	return &Room{
		ID: id, Name: name, passwordHash: passwordHash,
		st: st, authm: authm, cache: c, reg: reg,
		inbound: make(chan action, 64),
	}
}

// CheckPassword 校验访客密码（空密码房间直接放行）。
func (r *Room) CheckPassword(password string) bool {
	if r.passwordHash == "" {
		return true
	}
	return bcrypt.CompareHashAndPassword([]byte(r.passwordHash), []byte(password)) == nil
}

func (r *Room) call(a action) error {
	a.result = make(chan error, 1)
	r.inbound <- a
	return <-a.result
}

func (r *Room) Join(c ClientConn)    { r.inbound <- action{kind: actJoin, client: c} }
func (r *Room) Leave(c ClientConn)   { r.inbound <- action{kind: actLeave, client: c} }

func (r *Room) Add(e QueueEntry) error      { return r.call(action{kind: actAdd, entry: e}) }
func (r *Room) Remove(entryID string) error { return r.call(action{kind: actRemove, entryID: entryID}) }

// RemoveFor 带权限校验的移除：room_admin 或条目所有者可移除。
// 校验在 actor 内做，与队列状态读取天然串行，无竞态。
func (r *Room) RemoveFor(id auth.Identity, entryID string) error {
	a := action{kind: actRemove, entryID: entryID, actor: id}
	return r.call(a)
}
func (r *Room) Move(entryID string, to int) error {
	return r.call(action{kind: actMove, entryID: entryID, toIndex: to})
}
func (r *Room) Pause() error         { return r.call(action{kind: actPause}) }
func (r *Room) Resume() error        { return r.call(action{kind: actResume}) }
func (r *Room) SeekTo(posMs int64) error { return r.call(action{kind: actSeek, posMs: posMs}) }
func (r *Room) Skip() error          { return r.call(action{kind: actSkip}) }

// Run 是 actor 主循环。阻塞直到进程退出。
func (r *Room) Run(ctx context.Context) {
	queue := r.loadQueue()
	var playback *Playback
	clients := map[string]ClientConn{}

	var timer *time.Timer
	var timerSeq uint64 // 防止过期 timer 回调在新状态上触发

	nowMs := func() int64 { return time.Now().UnixMilli() }

	// position 由五元组推算此刻位置（毫秒）
	position := func(pb *Playback, now int64) int64 {
		if pb.Playing {
			return pb.PositionMs + int64(float64(now-pb.UpdatedAt)*pb.Rate)
		}
		return pb.PositionMs
	}

	persistQueue := func() {
		rows := make([]store.QueueRow, len(queue))
		for i, e := range queue {
			rows[i] = store.QueueRow{
				EntryID: e.EntryID, TrackRef: e.TrackRef, Title: e.Title,
				Artist: e.Artist, DurationMs: e.DurationMs,
				RequestedBy: e.RequestedBy, AddedAt: e.AddedAt,
			}
		}
		// DB 写入很快，actor 内同步执行换取简单的一致性
		_ = r.st.ReplaceQueue(ctx, r.ID, rows)
	}

	broadcast := func(build func(c ClientConn) any) {
		for _, c := range clients {
			c.Send(build(c))
		}
	}

	// playbackMsg 生成携带该客户端专属 stream_url 的 playback.changed
	playbackMsg := func(c ClientConn) any {
		if playback == nil {
			playback = &Playback{Rate: 1.0}
		}
		pb := *playback
		if pb.Current != nil {
			cur := *pb.Current
			cur.StreamURL = r.streamURL(c.Identity(), cur.TrackRef)
			pb.Current = &cur
		}
		return map[string]any{"type": "playback.changed", "data": pb}
	}

	queueMsg := func() any {
		return map[string]any{"type": "queue.changed", "data": map[string]any{"queue": queue}}
	}

	listenersMsg := func() any {
		type listener struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		ls := make([]listener, 0, len(clients))
		for _, c := range clients {
			id := c.Identity()
			ls = append(ls, listener{ID: id.ID, Name: id.Name})
		}
		return map[string]any{"type": "listeners.changed", "data": map[string]any{"listeners": ls}}
	}

	// scheduleEnd 按剩余时长设置自然结束 timer
	scheduleEnd := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		if playback == nil || !playback.Playing || playback.Current == nil || playback.Current.DurationMs <= 0 {
			return
		}
		remaining := playback.Current.DurationMs - position(playback, nowMs())
		if remaining < 0 {
			remaining = 0
		}
		timerSeq++
		seq := timerSeq
		timer = time.AfterFunc(time.Duration(remaining)*time.Millisecond, func() {
			r.inbound <- action{kind: actTimerEnd, timerID: seq}
		})
	}

	// finishCurrent 记录历史并清空当前曲目，返回是否有当前曲目
	finishCurrent := func(reason string, now int64) {
		if playback == nil || playback.Current == nil {
			return
		}
		cur := playback.Current
		_ = r.st.AddPlayHistory(ctx, r.ID, cur.TrackRef, cur.Title, cur.RequestedBy,
			cur.AddedAt, now, reason)
		r.authm.RevokeTrack(cur.TrackRef)
	}

	// advance 切到队首（或进入空闲）
	advance := func(reason string) {
		now := nowMs()
		finishCurrent(reason, now)
		if len(queue) == 0 {
			playback = &Playback{Rate: 1.0}
			if timer != nil {
				timer.Stop()
				timer = nil
			}
			broadcast(playbackMsg)
			return
		}
		next := queue[0]
		queue = queue[1:]
		persistQueue()
		playback = &Playback{
			Current: &next, PositionMs: 0, UpdatedAt: now, Playing: true, Rate: 1.0,
		}
		// 预取当前曲目（通常 Prefetch 已做过）与下一首
		if len(queue) > 0 {
			go r.cache.Prefetch(provider.TrackRef(queue[0].TrackRef))
		}
		scheduleEnd()
		broadcast(playbackMsg)
		broadcast(func(ClientConn) any { return queueMsg() })
	}

	for {
		select {
		case <-ctx.Done():
			return
		case a := <-r.inbound:
			now := nowMs()
			switch a.kind {
			case actJoin:
				clients[a.client.ID()] = a.client
				// 给新成员发全量快照：playback（含其专属票据）、队列、听众
				a.client.Send(playbackMsg(a.client))
				a.client.Send(queueMsg())
				broadcast(func(ClientConn) any { return listenersMsg() })

			case actLeave:
				delete(clients, a.client.ID())
				broadcast(func(ClientConn) any { return listenersMsg() })

			case actAdd:
				queue = append(queue, a.entry)
				persistQueue()
				if playback == nil || playback.Current == nil {
					advance("") // 空闲时自动开播（内部会广播 queue + playback）
				} else {
					broadcast(func(ClientConn) any { return queueMsg() })
					if len(queue) == 1 {
						// 这是队首候补，预热缓存
						go r.cache.Prefetch(provider.TrackRef(a.entry.TrackRef))
					}
				}
				a.result <- nil

			case actRemove:
				idx := -1
				for i, e := range queue {
					if e.EntryID == a.entryID {
						idx = i
						break
					}
				}
				if idx < 0 {
					a.result <- ErrEntryNotFound
					break
				}
				if !a.actor.HasRole(auth.RoleRoomAdmin) && queue[idx].RequestedBy != a.actor.ID {
					a.result <- ErrForbidden
					break
				}
				queue = append(queue[:idx], queue[idx+1:]...)
				persistQueue()
				broadcast(func(ClientConn) any { return queueMsg() })
				a.result <- nil

			case actMove:
				if a.toIndex < 0 || a.toIndex >= len(queue) {
					a.result <- errors.New("to_index out of range")
					break
				}
				found := false
				for i, e := range queue {
					if e.EntryID == a.entryID {
						queue = append(queue[:i], queue[i+1:]...)
						rest := append([]QueueEntry{}, queue[a.toIndex:]...)
						queue = append(append(queue[:a.toIndex], e), rest...)
						found = true
						break
					}
				}
				if !found {
					a.result <- ErrEntryNotFound
					break
				}
				persistQueue()
				broadcast(func(ClientConn) any { return queueMsg() })
				a.result <- nil

			case actPause:
				if playback == nil || playback.Current == nil || !playback.Playing {
					a.result <- ErrNoPlayback
					break
				}
				playback.PositionMs = position(playback, now)
				playback.Playing = false
				playback.UpdatedAt = now
				if timer != nil {
					timer.Stop()
					timer = nil
				}
				broadcast(playbackMsg)
				a.result <- nil

			case actResume:
				if playback == nil || playback.Current == nil || playback.Playing {
					a.result <- ErrNoPlayback
					break
				}
				playback.Playing = true
				playback.UpdatedAt = now
				scheduleEnd()
				broadcast(playbackMsg)
				a.result <- nil

			case actSeek:
				if playback == nil || playback.Current == nil {
					a.result <- ErrNoPlayback
					break
				}
				p := a.posMs
				if p < 0 {
					p = 0
				}
				if d := playback.Current.DurationMs; d > 0 && p > d {
					p = d
				}
				playback.PositionMs = p
				playback.UpdatedAt = now
				scheduleEnd()
				broadcast(playbackMsg)
				a.result <- nil

			case actSkip:
				advance("skipped")
				a.result <- nil

			case actTimerEnd:
				if a.timerID != timerSeq {
					break // 过期 timer，忽略
				}
				advance("finished")
			}
		}
	}
}

// Snapshot 供 REST 侧展示房间状态（非实时路径）。
func (r *Room) streamURL(id auth.Identity, trackRef string) string {
	ticket := r.authm.IssueTicket(id.ID, trackRef)
	return "/stream/v1/" + trackRef + "?ticket=" + ticket
}

func (r *Room) loadQueue() []QueueEntry {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := r.st.LoadQueue(ctx, r.ID)
	if err != nil {
		return nil
	}
	out := make([]QueueEntry, len(rows))
	for i, row := range rows {
		out[i] = QueueEntry{
			EntryID: row.EntryID, TrackRef: row.TrackRef, Title: row.Title,
			Artist: row.Artist, DurationMs: row.DurationMs,
			RequestedBy: row.RequestedBy, AddedAt: row.AddedAt,
		}
	}
	return out
}

// NewEntryID 生成队列条目 ID。
func NewEntryID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "e_" + hex.EncodeToString(b)
}
