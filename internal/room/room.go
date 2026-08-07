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
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

// ClientConn 是房间对 WS 客户端的抽象（由 wsapi 实现，避免包环依赖）。
type ClientConn interface {
	ID() string
	Identity() auth.Identity
	// Send 非阻塞；客户端发送缓冲满时由实现方断开。
	Send(msg any)
	// Interests 声明本连接要订阅的房间状态流。
	// 普通 Client / Integration 会话应返回 InterestAll；
	// Headless Player 只返回 InterestPlayback。
	Interests() RoomInterest
}

// RoomInterest 是房间状态广播的订阅位集。
type RoomInterest uint8

const (
	InterestPlayback RoomInterest = 1 << iota
	InterestQueue
	InterestRadio
	InterestListeners

	// InterestAll 是普通会话的默认订阅：完整房间状态。
	InterestAll = InterestPlayback | InterestQueue | InterestRadio | InterestListeners
)

// Has 报告 interest 是否包含 flag 中的全部位。
func (i RoomInterest) Has(flag RoomInterest) bool {
	return i&flag == flag
}

// QueueEntry 队列条目。StreamURL 仅在下发给具体客户端时填充（按身份签发票据）。
// Album/CoverURL/SourceURL/Contributors 为曲目层富字段（入队时快照）；
// CoverURL 存源站原始地址，广播时改写为服务端代理路径。
// SizeBytes/BitrateKbps 仅当前播放条目在广播时填充（物理层，来自缓存索引）。
type QueueEntry struct {
	EntryID       string                 `json:"entry_id"`
	TrackRef      string                 `json:"track_ref"`
	Title         string                 `json:"title"`
	Artist        string                 `json:"artist"`
	DurationMs    int64                  `json:"duration_ms"`
	Album         string                 `json:"album,omitempty"`
	CoverURL      string                 `json:"cover_url,omitempty"`
	SourceURL     string                 `json:"source_url,omitempty"`
	Contributors  []provider.Contributor `json:"contributors,omitempty"`
	RequestedBy   string                 `json:"requested_by"`
	RequesterName string                 `json:"requester_name"`
	AddedAt       int64                  `json:"added_at"`
	StreamURL     string                 `json:"stream_url,omitempty"`
	SizeBytes     int64                  `json:"size_bytes,omitempty"`
	BitrateKbps   int                    `json:"bitrate_kbps,omitempty"`
}

// EntryFromTrack 以入队快照语义从 Track 构造队列条目。
func EntryFromTrack(t provider.Track, requestedBy string) QueueEntry {
	return QueueEntry{
		EntryID: NewEntryID(), TrackRef: t.Ref.String(), Title: t.Title,
		Artist: t.Artist, DurationMs: t.DurationMs, Album: t.Album,
		CoverURL: t.CoverURL, SourceURL: t.SourceURL,
		Contributors: append([]provider.Contributor(nil), t.Contributors...),
		RequestedBy:  requestedBy, AddedAt: time.Now().UnixMilli(),
	}
}

// Playback 播放状态五元组（权威）。
//
// PositionMs 可以为负：切歌时房间把新曲目的 position 0 排在未来
// 「UpdatedAt + |PositionMs|」时刻（起播提前量，见 Policy.StartLeadMs），
// 推算出的 should_be 在这段窗口内为负，语义是「还有 |should_be| ms 开播」。
// 客户端应在此期间完成装载并保持暂停，到点开声；渲染进度条时钳到 0。
type Playback struct {
	Current    *QueueEntry `json:"current"`
	PositionMs int64       `json:"position_ms"`
	UpdatedAt  int64       `json:"updated_at"`
	Playing    bool        `json:"playing"`
	Rate       float64     `json:"rate"`
}

// DefaultStartLeadMs 默认起播提前量。取值覆盖典型的「WS 投递 + HTTP 首字节 +
// demux + 音频设备灌注」链路：低于此值仍会吃掉头部，高于此值曲间静默开始可闻。
const DefaultStartLeadMs = 600

// NowPlayingSummary 是大厅目录可公开的当前播放裁剪信息。
// 它不含 track_ref、requested_by 或按身份签发的 stream_url。
// PositionMs 与 Playback 同源，起播提前量窗口内为负——渲染方钳到 0。
type NowPlayingSummary struct {
	Title      string  `json:"title"`
	Artist     string  `json:"artist"`
	DurationMs int64   `json:"duration_ms"`
	CoverURL   string  `json:"cover_url"`
	PositionMs int64   `json:"position_ms"`
	UpdatedAt  int64   `json:"updated_at"`
	Playing    bool    `json:"playing"`
	Rate       float64 `json:"rate"`
}

// DirectorySnapshot 是 actor 内存态在大厅目录中的非敏感摘要。
type DirectorySnapshot struct {
	ListenerCount int
	NowPlaying    *NowPlayingSummary
}

// ListenerSnapshot 是房间快照中的已连接听众。
type ListenerSnapshot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// RadioSnapshot 是房间当前电台绑定的只读投影。
type RadioSnapshot struct {
	Source      string `json:"source"`
	Description string `json:"description"`
	Finite      bool   `json:"finite"`
	Shuffle     bool   `json:"shuffle"`
	Once        bool   `json:"once"`
}

// Snapshot 是与进房广播同源的房间完整状态。返回值不引用 actor 内部可变数据。
type Snapshot struct {
	Playback  Playback           `json:"playback"`
	Queue     []QueueEntry       `json:"queue"`
	Radio     *RadioSnapshot     `json:"radio"`
	Listeners []ListenerSnapshot `json:"listeners"`
}

type actKind int

const (
	actJoin actKind = iota
	actLeave
	actQueueSync
	actAdd
	actRemove
	actMove
	actClear
	actPause
	actResume
	actSeek
	actSkip
	actTimerEnd
	actUnplayable // 预检失败：当前曲目无可播地址，自动跳过
	actRadioPlay
	actRadioStop
	actSetPolicy
	actDirectorySnapshot
	actSnapshot
)

type action struct {
	kind      actKind
	client    ClientConn
	actor     auth.Identity
	entry     QueueEntry
	entries   []QueueEntry // 批量入队（actAdd；非空时优先于 entry）
	entryID   string
	removeAny bool
	toIndex   int
	posMs     int64
	timerID   uint64
	ref       string // actUnplayable：预检失败的 track_ref
	// radio
	source           string
	shuffle          bool
	once             bool
	policyRaw        string
	result           chan error
	snapshot         chan DirectorySnapshot
	stateSnapshot    chan Snapshot
	includeStreamURL bool
}

var (
	ErrQueueEmpty        = errors.New("queue is empty")
	ErrEntryNotFound     = errors.New("queue entry not found")
	ErrInvalidQueueIndex = errors.New("to_index out of range")
	ErrNoPlayback        = errors.New("nothing is playing")
	ErrForbidden         = errors.New("forbidden")
)

// radioState 电台模式状态（运行时，不落库）。
type radioState struct {
	src     provider.TrackSource
	spec    string
	shuffle bool
	once    bool
}

type Room struct {
	ID        string
	Name      string
	policyRaw string
	access    *roomAccess

	st    *store.Store
	authm *auth.Manager
	cache *cache.Cache
	reg   *provider.Registry

	inbound chan action
	done    chan struct{} // Run 退出时关闭，阻断后续调用
}

func New(id, name, passwordHash, policyRaw string, st *store.Store, authm *auth.Manager, c *cache.Cache, reg *provider.Registry) *Room {
	mode := AccessModeOpen
	if passwordHash != "" {
		mode = AccessModeStaticPassword
	}
	return newPersistentRoom(store.Room{
		ID: id, Name: name, PasswordHash: passwordHash, AccessMode: string(mode),
		CodePeriodSeconds: DefaultCodePeriodSeconds, PolicyJSON: policyRaw,
	}, nil, st, authm, c, reg)
}

func newPersistentRoom(row store.Room, codeKey []byte, st *store.Store, authm *auth.Manager, c *cache.Cache, reg *provider.Registry) *Room {
	mode := AccessMode(row.AccessMode)
	if mode == "" {
		mode = AccessModeOpen
		if row.PasswordHash != "" {
			mode = AccessModeStaticPassword
		}
	}
	config := AccessConfig{
		Mode: mode, PasswordHash: row.PasswordHash, CodePeriodSeconds: row.CodePeriodSeconds,
		TrustedRoles: row.TrustedRoles,
	}
	return &Room{
		ID: row.ID, Name: row.Name, policyRaw: row.PolicyJSON,
		access: newRoomAccess(row.ID, row.CreatedAt, codeKey, config),
		st:     st, authm: authm, cache: c, reg: reg,
		inbound: make(chan action, 64),
		done:    make(chan struct{}),
	}
}

// PolicyRaw 当前策略 JSON（供 REST 展示）。
func (r *Room) PolicyRaw() string { return r.policyRaw }

// CheckAccessCredential validates the credential required to enter this Room.
func (r *Room) CheckAccessCredential(credential string) bool {
	return r.access.check(credential, time.Now())
}

func (r *Room) AccessConfig() AccessConfig {
	return r.access.load()
}

// ApplyAccessConfig makes a validated, persisted access configuration live.
func (r *Room) ApplyAccessConfig(config AccessConfig) error {
	return r.access.set(config)
}

func (r *Room) CurrentAccessCode() (AccessCode, error) {
	return r.access.currentCode(time.Now())
}

// ErrRoomClosed 房间 actor 已停止（房间被删除）。
var ErrRoomClosed = errors.New("room closed")

func (r *Room) call(a action) error {
	a.result = make(chan error, 1)
	select {
	case r.inbound <- a:
	case <-r.done:
		return ErrRoomClosed
	}
	select {
	case err := <-a.result:
		return err
	case <-r.done:
		return ErrRoomClosed
	}
}

func (r *Room) Join(c ClientConn) {
	select {
	case r.inbound <- action{kind: actJoin, client: c}:
	case <-r.done:
	}
}

// SyncQueue serializes a fresh revisioned queue baseline through the actor.
func (r *Room) SyncQueue(c ClientConn) error {
	return r.call(action{kind: actQueueSync, client: c})
}

func (r *Room) Leave(c ClientConn) {
	select {
	case r.inbound <- action{kind: actLeave, client: c}:
	case <-r.done:
	}
}

// AddFor 带策略校验的点歌：队列总上限与按身份（kind/role）的待播上限
// 在 actor 内检查，与队列状态读取天然串行。电台补充不经过此路径。
func (r *Room) AddFor(id auth.Identity, e QueueEntry) error {
	return r.call(action{kind: actAdd, actor: id, entry: e})
}

// AddBatchFor 原子批量点歌：整体预校验（max_queue 与按身份限额均按批量后
// 投影值计算），任一不通过则一条不加；全部通过按顺序队尾追加。
func (r *Room) AddBatchFor(id auth.Identity, entries []QueueEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return r.call(action{kind: actAdd, actor: id, entries: entries})
}

// Remove 无条件移除待播条目。调用方必须已经完成 controller 授权。
func (r *Room) Remove(entryID string) error {
	return r.call(action{kind: actRemove, entryID: entryID, removeAny: true})
}

// RemoveFor 仅允许条目所有者移除。controller 授权由 control.Service 统一处理。
// 所有权校验在 actor 内完成，与队列状态读取天然串行。
func (r *Room) RemoveFor(id auth.Identity, entryID string) error {
	return r.call(action{kind: actRemove, entryID: entryID, actor: id})
}
func (r *Room) Move(entryID string, to int) error {
	return r.call(action{kind: actMove, entryID: entryID, toIndex: to})
}

// ClearQueue atomically clears the pending queue. Playback and radio binding are unchanged.
func (r *Room) ClearQueue() error {
	return r.call(action{kind: actClear})
}
func (r *Room) Pause() error             { return r.call(action{kind: actPause}) }
func (r *Room) Resume() error            { return r.call(action{kind: actResume}) }
func (r *Room) SeekTo(posMs int64) error { return r.call(action{kind: actSeek, posMs: posMs}) }
func (r *Room) Skip() error              { return r.call(action{kind: actSkip}) }

// PlayRadio 让房间进入电台模式：绑定曲目源，队列见底自动补充。
func (r *Room) PlayRadio(sourceSpec string, shuffle, once bool) error {
	return r.call(action{kind: actRadioPlay, source: sourceSpec, shuffle: shuffle, once: once})
}

// StopRadio 退出电台模式（队列已有内容继续播）。
func (r *Room) StopRadio() error { return r.call(action{kind: actRadioStop}) }

// SetPolicy 热更新房间策略：校验 → 落库 → actor 内生效。
func (r *Room) SetPolicy(raw string) error {
	return r.call(action{kind: actSetPolicy, policyRaw: raw})
}

// DirectorySnapshot 串行读取 actor 的听众数与播放五元组摘要。
func (r *Room) DirectorySnapshot() (DirectorySnapshot, error) {
	ch := make(chan DirectorySnapshot, 1)
	select {
	case r.inbound <- action{kind: actDirectorySnapshot, snapshot: ch}:
	case <-r.done:
		return DirectorySnapshot{}, ErrRoomClosed
	}
	select {
	case snapshot := <-ch:
		return snapshot, nil
	case <-r.done:
		return DirectorySnapshot{}, ErrRoomClosed
	}
}

// Snapshot 同步读取与进房广播同源的完整状态，但不把查询者加入听众。
func (r *Room) Snapshot(id auth.Identity) (Snapshot, error) {
	return r.snapshot(id, true)
}

// SnapshotWithoutStreamURL returns the same projection without issuing a stream
// ticket. The control boundary uses it for callers not admitted to protected rooms.
func (r *Room) SnapshotWithoutStreamURL(id auth.Identity) (Snapshot, error) {
	return r.snapshot(id, false)
}

func (r *Room) snapshot(id auth.Identity, includeStreamURL bool) (Snapshot, error) {
	ch := make(chan Snapshot, 1)
	select {
	case r.inbound <- action{
		kind: actSnapshot, actor: id, stateSnapshot: ch, includeStreamURL: includeStreamURL,
	}:
	case <-r.done:
		return Snapshot{}, ErrRoomClosed
	}
	select {
	case snapshot := <-ch:
		return snapshot, nil
	case <-r.done:
		return Snapshot{}, ErrRoomClosed
	}
}

// Run 是 actor 主循环。阻塞直到 ctx 取消（进程退出或房间被删除）。
func (r *Room) Run(ctx context.Context) {
	defer close(r.done)
	queue, resumed := r.loadQueue()
	var queueRevision uint64
	var playback *Playback
	var radio *radioState
	clients := map[string]ClientConn{}
	policy, err := ParsePolicy(r.policyRaw)
	if err != nil {
		log.Printf("room %s: invalid stored policy, using empty: %v", r.ID, err)
	}

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

	queueRows := func(next []QueueEntry) ([]store.QueueRow, error) {
		rows := make([]store.QueueRow, len(next))
		for i, e := range next {
			contrib, err := json.Marshal(e.Contributors)
			if err != nil {
				return nil, err
			}
			rows[i] = store.QueueRow{
				EntryID: e.EntryID, TrackRef: e.TrackRef, Title: e.Title,
				Artist: e.Artist, DurationMs: e.DurationMs,
				Album: e.Album, CoverURL: e.CoverURL, SourceURL: e.SourceURL,
				ContributorsJSON: string(contrib),
				RequestedBy:      e.RequestedBy, AddedAt: e.AddedAt,
				RequesterName: e.RequesterName,
			}
		}
		return rows, nil
	}

	// broadcast 只推送给声明了对应 interest 的连接。
	// 新状态流必须选明确 interest，避免默认打到所有连接。
	broadcast := func(interest RoomInterest, build func(c ClientConn) any) {
		for _, c := range clients {
			if !c.Interests().Has(interest) {
				continue
			}
			c.Send(build(c))
		}
	}

	cloneEntry := func(entry QueueEntry) QueueEntry {
		cloned := entry
		cloned.Contributors = append([]provider.Contributor(nil), entry.Contributors...)
		return cloned
	}

	playbackSnapshot := func(id auth.Identity, includeStreamURL bool) Playback {
		pb := Playback{Rate: 1.0}
		if playback != nil {
			pb = *playback
		}
		if pb.Current != nil {
			cur := cloneEntry(*pb.Current)
			if includeStreamURL {
				cur.StreamURL = r.streamURL(id, cur.TrackRef)
			} else {
				cur.StreamURL = ""
			}
			if cur.CoverURL != "" {
				cur.CoverURL = "/api/v1/cover/" + url.PathEscape(cur.TrackRef)
			}
			pb.Current = &cur
		}
		return pb
	}

	queueEntrySnapshot := func(entry QueueEntry) QueueEntry {
		entry = cloneEntry(entry)
		if entry.CoverURL != "" {
			entry.CoverURL = "/api/v1/cover/" + url.PathEscape(entry.TrackRef)
		}
		return entry
	}

	queueSnapshot := func() []QueueEntry {
		q := make([]QueueEntry, len(queue))
		for i, entry := range queue {
			q[i] = queueEntrySnapshot(entry)
		}
		return q
	}

	sendQueueSnapshot := func(c ClientConn) error {
		messages, err := QueueSnapshotMessages(queueRevision, queueSnapshot())
		if err != nil {
			return err
		}
		for _, message := range messages {
			c.Send(message)
		}
		return nil
	}

	wireQueuePatchOps := func(ops []QueuePatchOp) []QueuePatchOp {
		wire := make([]QueuePatchOp, len(ops))
		for i, op := range ops {
			wire[i] = op
			if op.Item != nil {
				item := queueEntrySnapshot(*op.Item)
				wire[i].Item = &item
			}
		}
		return wire
	}

	// persistQueue 把「当前曲目 + 待播队列」整体落库。当前曲目保留在 room_queue 中
	// 并由 current_entry_id 标记游标，这样它对 SQL 可见（加速层可以钉住正在流式
	// 传输的对象），重启也能从它本身续播。线上的 queue 表示不变，仍只含待播条目。
	persistQueue := func(current *QueueEntry, next []QueueEntry) error {
		entries, currentEntryID := next, ""
		if current != nil {
			currentEntryID = current.EntryID
			entries = append([]QueueEntry{*current}, next...)
		}
		rows, err := queueRows(entries)
		if err != nil {
			return err
		}
		return r.st.ReplaceQueue(ctx, r.ID, currentEntryID, rows)
	}

	// commitQueueWithCurrent 在切歌时使用：此刻游标要指向新曲目，而 playback
	// 尚未推进（推进前先落库，失败时房间状态保持不变）。
	commitQueueWithCurrent := func(current *QueueEntry, next []QueueEntry, ops []QueuePatchOp) error {
		nextRevision := queueRevision + 1
		messages, err := QueuePatchMessages(queueRevision, nextRevision, wireQueuePatchOps(ops))
		if err != nil {
			return err
		}
		if err := persistQueue(current, next); err != nil {
			return err
		}
		queue = next
		queueRevision = nextRevision
		for _, message := range messages {
			message := message
			broadcast(InterestQueue, func(ClientConn) any { return message })
		}
		return nil
	}

	// commitQueue 用于不改变当前曲目的队列变更（增删移清）。
	commitQueue := func(next []QueueEntry, ops []QueuePatchOp) error {
		var current *QueueEntry
		if playback != nil {
			current = playback.Current
		}
		return commitQueueWithCurrent(current, next, ops)
	}

	queueAddOps := func(entries []QueueEntry, start int) []QueuePatchOp {
		ops := make([]QueuePatchOp, len(entries))
		for i := range entries {
			item := entries[i]
			ops[i] = QueuePatchOp{Op: QueueOpAdd, Index: start + i, Item: &item}
		}
		return ops
	}

	queueRemoveOps := func(entries []QueueEntry) []QueuePatchOp {
		ops := make([]QueuePatchOp, len(entries))
		for i, entry := range entries {
			ops[i] = QueuePatchOp{Op: QueueOpRemove, EntryID: entry.EntryID}
		}
		return ops
	}

	listenersSnapshot := func() []ListenerSnapshot {
		listeners := make([]ListenerSnapshot, 0, len(clients))
		for _, c := range clients {
			if !c.Interests().Has(InterestListeners) {
				continue
			}
			id := c.Identity()
			listeners = append(listeners, ListenerSnapshot{ID: id.ID, Name: id.Name})
		}
		return listeners
	}

	radioSnapshot := func() *RadioSnapshot {
		if radio == nil {
			return nil
		}
		return &RadioSnapshot{
			Source: radio.spec, Description: radio.src.Description(), Finite: radio.src.Finite(),
			Shuffle: radio.shuffle, Once: radio.once,
		}
	}

	snapshotFor := func(id auth.Identity, includeStreamURL bool) Snapshot {
		return Snapshot{
			Playback:  playbackSnapshot(id, includeStreamURL),
			Queue:     queueSnapshot(),
			Radio:     radioSnapshot(),
			Listeners: listenersSnapshot(),
		}
	}

	playbackMsg := func(c ClientConn) any {
		return map[string]any{"type": "playback.changed", "data": playbackSnapshot(c.Identity(), true)}
	}

	listenersMsg := func() any {
		return map[string]any{
			"type": "listeners.changed",
			"data": map[string]any{"listeners": listenersSnapshot()},
		}
	}

	radioMsg := func() any {
		return map[string]any{"type": "radio.changed", "data": map[string]any{"radio": radioSnapshot()}}
	}

	// stopRadio 退出电台并广播
	stopRadio := func() {
		radio = nil
		broadcast(InterestRadio, func(ClientConn) any { return radioMsg() })
	}

	// refill 电台补充：从源取一批曲目追加到队列，但绝不超过 max_queue。
	// 源耗尽（once 语义）或出错时停止电台。
	refill := func() error {
		if radio == nil {
			return nil
		}
		batchSize := 10
		if policy.MaxQueue > 0 {
			batchSize = min(batchSize, policy.MaxQueue-len(queue))
			if batchSize <= 0 {
				return nil
			}
		}
		seed := provider.TrackRef("")
		if playback != nil && playback.Current != nil {
			seed = provider.TrackRef(playback.Current.TrackRef)
		}
		bctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		tracks, exhausted, err := radio.src.NextBatch(bctx, batchSize, seed)
		cancel()
		if err != nil {
			log.Printf("[room %s] radio %s: %v (stopping)", r.ID, radio.spec, err)
			stopRadio()
			return nil
		}
		if len(tracks) > batchSize {
			tracks = tracks[:batchSize]
			exhausted = false
		}
		if len(tracks) > 0 {
			entries := make([]QueueEntry, len(tracks))
			for i, track := range tracks {
				entries[i] = EntryFromTrack(track, "radio")
			}
			start := len(queue)
			next := make([]QueueEntry, start+len(entries))
			copy(next, queue)
			copy(next[start:], entries)
			if err := commitQueue(next, queueAddOps(entries, start)); err != nil {
				return err
			}
		}
		if exhausted {
			stopRadio()
		}
		return nil
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
		pid, id, err := provider.TrackRef(cur.TrackRef).Split()
		if err != nil {
			return
		}
		p, ok := r.reg.Get(pid)
		if !ok {
			return
		}
		rep, ok := p.(provider.PlayReporter)
		if !ok {
			return
		}
		owner, ok, err := r.st.GetCredentialOwner(ctx, pid)
		if err != nil || !ok || owner.PrincipalID == "" || cur.RequestedBy != owner.PrincipalID {
			return
		}

		playedMs := position(playback, now)
		if playedMs < 0 {
			playedMs = 0
		}
		totalMs := cur.DurationMs
		threshold := int64(240_000)
		if totalMs > 0 && totalMs/2 < threshold {
			threshold = totalMs / 2
		}
		if playedMs < threshold {
			return
		}

		principalID := cur.RequestedBy
		target := cur.TrackRef
		roomID := r.ID
		st := r.st
		go func(
			rep provider.PlayReporter,
			id string,
			playedMs, totalMs int64,
			principalID, target, roomID string,
		) {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := rep.ReportPlay(cctx, id, playedMs, totalMs)
			cancel()
			if err != nil {
				log.Printf("[room %s] scrobble %s: %v", roomID, target, err)
			}
			detailValue := struct {
				PlayedMs int64  `json:"played_ms"`
				TotalMs  int64  `json:"total_ms"`
				OK       bool   `json:"ok"`
				Error    string `json:"error,omitempty"`
			}{
				PlayedMs: playedMs,
				TotalMs:  totalMs,
				OK:       err == nil,
			}
			if err != nil {
				detailValue.Error = err.Error()
			}
			detail, _ := json.Marshal(detailValue)
			_ = st.Audit(context.Background(), principalID, "provider.scrobble", target, string(detail))
		}(rep, id, playedMs, totalMs, principalID, target, roomID)
	}

	// advance 切到队首（或进入空闲）；电台模式下队列见底自动补充。
	// 毒化条目（时长未知，如元数据缺失的历史遗留）直接丢弃——
	// 它们会让 scheduleEnd 永不触发，队列永久停滞。
	//
	// 新曲目的 position 起于 -startLeadMs：房间时间线在曲间留出一段提前量，
	// 客户端用它装载解码，到 position 0 时同时开声。没有这段窗口时，
	// 客户端装载完成时房间已经走过几百毫秒，只能 seek 过去——头部固定丢失。
	advance := func(reason string) error {
		if len(queue) == 0 && radio != nil {
			if err := refill(); err != nil {
				return err
			}
		}

		now := nowMs()
		if len(queue) == 0 {
			// 先清掉游标再记历史：落库失败时房间状态保持不变。
			if err := persistQueue(nil, nil); err != nil {
				return err
			}
			finishCurrent(reason, now)
			playback = &Playback{Rate: 1.0}
			if timer != nil {
				timer.Stop()
				timer = nil
			}
			broadcast(InterestPlayback, playbackMsg)
			return nil
		}

		removeCount := 0
		var next QueueEntry
		hasNext := false
		for removeCount < len(queue) && removeCount < 100 {
			candidate := queue[removeCount]
			removeCount++
			if candidate.DurationMs > 0 {
				next = candidate
				hasNext = true
				break
			}
		}
		removed := append([]QueueEntry(nil), queue[:removeCount]...)
		remaining := append([]QueueEntry(nil), queue[removeCount:]...)
		var nextCurrent *QueueEntry
		if hasNext {
			nextCurrent = &next
		}
		if err := commitQueueWithCurrent(nextCurrent, remaining, queueRemoveOps(removed)); err != nil {
			return err
		}

		finishCurrent(reason, now)
		for _, entry := range removed {
			if entry.DurationMs <= 0 {
				log.Printf("room %s: dropping entry %s (%s): zero/unknown duration", r.ID, entry.EntryID, entry.TrackRef)
			}
		}
		if !hasNext {
			// 每次最多丢弃 100 条，防止病态电台源持续产出无时长条目。
			playback = &Playback{Rate: 1.0}
			if timer != nil {
				timer.Stop()
				timer = nil
			}
			broadcast(InterestPlayback, playbackMsg)
			return nil
		}

		playback = &Playback{
			Current: &next, PositionMs: -policy.startLeadMs(), UpdatedAt: now, Playing: true, Rate: 1.0,
		}
		// 物理层质量信息：已缓存则立即可知（Resolve 已发生）
		if row, err := r.st.GetCacheRow(ctx, next.TrackRef); err == nil {
			next.SizeBytes = row.SizeBytes
			next.BitrateKbps = row.BitrateKbps
			playback.Current = &next
		}
		if radio != nil && len(queue) < 3 {
			if err := refill(); err != nil {
				log.Printf("room %s: persist radio refill: %v", r.ID, err)
			}
		}
		// 当前曲目就位：异步预检可播性（已缓存直接放行；Resolve 失败——
		// 如 qq 无凭据的 104003——回报 actor 自动跳过，不卡到时长耗尽）。
		go r.preflightCurrent(provider.TrackRef(next.TrackRef))
		// queue 此刻已不含当前曲目，队首就是下一首：提前把它拉进本地缓存。
		if len(queue) > 0 {
			go r.cache.Prefetch(provider.TrackRef(queue[0].TrackRef))
		}
		scheduleEnd()
		broadcast(InterestPlayback, playbackMsg)
		return nil
	}

	// 启动即恢复：重启后播放状态丢失，但当前曲目留在队列里（由 current_entry_id
	// 标记），所以可以从它本身续播，而不是像以前那样跳过它从下一首开始。
	// 时长未知的条目会让 scheduleEnd 永不触发，交给 advance 按毒化条目丢弃。
	switch {
	case resumed != nil && resumed.DurationMs > 0:
		now := nowMs()
		playback = &Playback{
			Current: resumed, PositionMs: -policy.startLeadMs(), UpdatedAt: now, Playing: true, Rate: 1.0,
		}
		go r.preflightCurrent(provider.TrackRef(resumed.TrackRef))
		go r.cache.Prefetch(provider.TrackRef(resumed.TrackRef))
		if len(queue) > 0 {
			go r.cache.Prefetch(provider.TrackRef(queue[0].TrackRef))
		}
		scheduleEnd()
		broadcast(InterestPlayback, playbackMsg)
	case resumed != nil || len(queue) > 0:
		if err := advance("resume-after-restart"); err != nil {
			log.Printf("room %s: resume queue: %v", r.ID, err)
		}
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
				// 按连接声明的 interest 推入房快照。
				// Headless Player 通常只订 InterestPlayback（含专属 stream ticket）。
				interest := a.client.Interests()
				if interest.Has(InterestPlayback) {
					a.client.Send(playbackMsg(a.client))
				}
				if interest.Has(InterestQueue) {
					if err := sendQueueSnapshot(a.client); err != nil {
						log.Printf("room %s: queue snapshot for %s: %v", r.ID, a.client.ID(), err)
						a.client.Send(map[string]any{
							"type": "error",
							"data": map[string]any{"code": "queue_snapshot_failed", "message": err.Error()},
						})
					}
				}
				if interest.Has(InterestRadio) {
					a.client.Send(radioMsg())
				}
				if interest.Has(InterestListeners) {
					broadcast(InterestListeners, func(ClientConn) any { return listenersMsg() })
				}

			case actQueueSync:
				if _, joined := clients[a.client.ID()]; !joined || !a.client.Interests().Has(InterestQueue) {
					a.result <- ErrForbidden
					break
				}
				a.result <- sendQueueSnapshot(a.client)

			case actLeave:
				wasListener := a.client.Interests().Has(InterestListeners)
				delete(clients, a.client.ID())
				if wasListener {
					broadcast(InterestListeners, func(ClientConn) any { return listenersMsg() })
				}

			case actAdd:
				entries := a.entries
				if len(entries) == 0 {
					entries = []QueueEntry{a.entry}
				}
				n := len(entries)
				// 原子预校验：上限按批量后投影值计算，任一不通过一条不加
				if policy.MaxQueue > 0 && len(queue)+n > policy.MaxQueue {
					a.result <- ErrQueueFull
					break
				}
				if lim := policy.queueLimit(a.actor); lim > 0 {
					pending := 0
					for _, e := range queue {
						if e.RequestedBy == a.actor.ID {
							pending++
						}
					}
					if pending+n > lim {
						a.result <- ErrQuotaExceeded
						break
					}
				}
				wasEmpty := len(queue) == 0
				firstAdded := len(queue)
				next := make([]QueueEntry, firstAdded+n)
				copy(next, queue)
				for i, entry := range entries {
					entry = cloneEntry(entry)
					entry.RequesterName = a.actor.Name
					next[firstAdded+i] = entry
				}
				added := next[firstAdded:]
				if err := commitQueue(next, queueAddOps(added, firstAdded)); err != nil {
					a.result <- err
					break
				}
				if playback == nil || playback.Current == nil {
					// 自动开播是独立的内部队列变化；失败时保留已成功提交的点歌。
					if err := advance(""); err != nil {
						log.Printf("room %s: auto-advance after add: %v", r.ID, err)
					}
				} else if wasEmpty {
					// 追加前队列为空：新条目即队首候补，预热缓存
					go r.cache.Prefetch(provider.TrackRef(queue[0].TrackRef))
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
				if !a.removeAny && queue[idx].RequestedBy != a.actor.ID {
					a.result <- ErrForbidden
					break
				}
				removed := queue[idx]
				next := make([]QueueEntry, 0, len(queue)-1)
				next = append(next, queue[:idx]...)
				next = append(next, queue[idx+1:]...)
				err := commitQueue(next, []QueuePatchOp{{Op: QueueOpRemove, EntryID: removed.EntryID}})
				a.result <- err

			case actMove:
				if a.toIndex < 0 || a.toIndex >= len(queue) {
					a.result <- ErrInvalidQueueIndex
					break
				}
				idx := -1
				for i, entry := range queue {
					if entry.EntryID == a.entryID {
						idx = i
						break
					}
				}
				if idx < 0 {
					a.result <- ErrEntryNotFound
					break
				}
				moved := queue[idx]
				without := make([]QueueEntry, 0, len(queue)-1)
				without = append(without, queue[:idx]...)
				without = append(without, queue[idx+1:]...)
				next := make([]QueueEntry, 0, len(queue))
				next = append(next, without[:a.toIndex]...)
				next = append(next, moved)
				next = append(next, without[a.toIndex:]...)
				err := commitQueue(next, []QueuePatchOp{{
					Op: QueueOpMove, EntryID: moved.EntryID, ToIndex: a.toIndex,
				}})
				a.result <- err

			case actClear:
				err := commitQueue([]QueueEntry{}, []QueuePatchOp{{Op: QueueOpClear}})
				a.result <- err

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
				broadcast(InterestPlayback, playbackMsg)
				a.result <- nil

			case actResume:
				if playback == nil || playback.Current == nil || playback.Playing {
					a.result <- ErrNoPlayback
					break
				}
				playback.Playing = true
				playback.UpdatedAt = now
				scheduleEnd()
				broadcast(InterestPlayback, playbackMsg)
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
				broadcast(InterestPlayback, playbackMsg)
				a.result <- nil

			case actSkip:
				a.result <- advance("skipped")

			case actTimerEnd:
				if a.timerID != timerSeq {
					break // 过期 timer，忽略
				}
				if err := advance("finished"); err != nil {
					log.Printf("room %s: timer advance: %v", r.ID, err)
				}

			case actUnplayable:
				// 预检失败：仅当该 ref 仍是当前曲目时跳过（竞态防护）。
				// a.result 可空（preflight goroutine 投递路径），nil 检查防阻塞。
				var skipErr error
				if playback != nil && playback.Current != nil && playback.Current.TrackRef == a.ref {
					skipErr = advance("unplayable")
					if skipErr != nil {
						log.Printf("room %s: unplayable advance: %v", r.ID, skipErr)
					}
				}
				if a.result != nil {
					a.result <- skipErr
				}

			case actRadioPlay:
				sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				src, err := NewSourceFromSpec(sctx, a.source, r.st, r.reg, a.shuffle, a.once)
				cancel()
				if err != nil {
					a.result <- err
					break
				}
				radio = &radioState{src: src, spec: a.source, shuffle: a.shuffle, once: a.once}
				broadcast(InterestRadio, func(ClientConn) any { return radioMsg() })
				if playback == nil || playback.Current == nil {
					err = advance("") // 空闲时电台立即开播
				} else if len(queue) < 3 {
					err = refill()
				}
				if err != nil {
					stopRadio()
					a.result <- err
					break
				}
				a.result <- nil

			case actRadioStop:
				stopRadio()
				a.result <- nil

			case actDirectorySnapshot:
				listenerCount := 0
				for _, c := range clients {
					if c.Interests().Has(InterestListeners) {
						listenerCount++
					}
				}
				snapshot := DirectorySnapshot{ListenerCount: listenerCount}
				if playback != nil && playback.Current != nil {
					cur := playback.Current
					coverURL := cur.CoverURL
					if coverURL != "" {
						coverURL = "/api/v1/cover/" + url.PathEscape(cur.TrackRef)
					}
					snapshot.NowPlaying = &NowPlayingSummary{
						Title: cur.Title, Artist: cur.Artist, DurationMs: cur.DurationMs,
						CoverURL: coverURL, PositionMs: playback.PositionMs,
						UpdatedAt: playback.UpdatedAt, Playing: playback.Playing, Rate: playback.Rate,
					}
				}
				a.snapshot <- snapshot

			case actSnapshot:
				a.stateSnapshot <- snapshotFor(a.actor, a.includeStreamURL)

			case actSetPolicy:
				p, err := ParsePolicy(a.policyRaw)
				if err != nil {
					a.result <- err
					break
				}
				if err := r.st.UpdateRoomPolicy(ctx, r.ID, a.policyRaw); err != nil {
					a.result <- err
					break
				}
				policy = p
				r.policyRaw = a.policyRaw
				a.result <- nil
			}
		}
	}
}

// streamURL 为当前观察者签发绑定身份与曲目的短期票据。
func (r *Room) streamURL(id auth.Identity, trackRef string) string {
	ticket := r.authm.IssueTicket(id.ID, trackRef)
	return "/stream/v1/" + url.PathEscape(trackRef) + "?ticket=" + ticket
}

// preflightCurrent 对刚就位的当前曲目做可播性预检：已缓存直接放行；
// provider Resolve 失败（源无效，如 qq 无凭据的 104003、local 文件缺失）
// 时回报 actor 自动跳过——否则房间会卡在无效曲目上直到时长耗尽。
// 竞态防护：回报携带 ref，actor 只跳过仍匹配当前曲目的情况
// （用户预检期间手动切走则不误伤新曲目）。
func (r *Room) preflightCurrent(ref provider.TrackRef) {
	if r.cache == nil {
		return
	}
	if r.cache.Lookup(context.Background(), ref) != "" {
		return // 已缓存，直接可播
	}
	pid, _, err := ref.Split()
	if err != nil {
		r.reportUnplayable(ref)
		return
	}
	p, ok := r.reg.Get(pid)
	if !ok {
		r.reportUnplayable(ref)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := p.Resolve(ctx, ref); err != nil {
		log.Printf("room %s: current %s unplayable: %v", r.ID, ref, err)
		r.reportUnplayable(ref)
	}
}

// reportUnplayable 把预检失败送回 actor（房间关闭时不阻塞）。
func (r *Room) reportUnplayable(ref provider.TrackRef) {
	select {
	case r.inbound <- action{kind: actUnplayable, ref: ref.String()}:
	case <-r.done:
	}
}

// loadQueue 读取持久队列，并按 current_entry_id 把当前曲目从待播队列中切分出来。
// 返回的队列与线上表示一致：只含待播条目。
func (r *Room) loadQueue() ([]QueueEntry, *QueueEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, currentEntryID, err := r.st.LoadQueue(ctx, r.ID)
	if err != nil {
		return nil, nil
	}
	out := make([]QueueEntry, len(rows))
	for i, row := range rows {
		var contributors []provider.Contributor
		if row.ContributorsJSON != "" {
			json.Unmarshal([]byte(row.ContributorsJSON), &contributors)
		}
		out[i] = QueueEntry{
			EntryID: row.EntryID, TrackRef: row.TrackRef, Title: row.Title,
			Artist: row.Artist, DurationMs: row.DurationMs,
			Album: row.Album, CoverURL: row.CoverURL, SourceURL: row.SourceURL,
			Contributors: contributors,
			RequestedBy:  row.RequestedBy, AddedAt: row.AddedAt,
			RequesterName: row.RequesterName,
		}
	}
	if currentEntryID == "" {
		return out, nil
	}
	for i := range out {
		if out[i].EntryID != currentEntryID {
			continue
		}
		current := out[i]
		return append([]QueueEntry(nil), out[i+1:]...), &current
	}
	// 游标指向的条目已不在队列里（异常漂移）：整队按待播处理，宁可重播一首也不丢队列。
	return out, nil
}

// NewEntryID 生成队列条目 ID。
func NewEntryID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "e_" + hex.EncodeToString(b)
}
