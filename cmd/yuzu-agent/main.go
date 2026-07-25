// Command yuzu-agent 是 MPV 播放代理：常驻进程，加入房间后把
// 服务器权威播放状态渲染到本地 MPV。纯收听，不提供控制能力。
//
// 同步策略（spec-v1 §2.2）：
//
//	|漂移| > 150ms  → 直接 seek
//	30–150ms        → 调 speed（0.98–1.02）缓追
//	< 30ms          → 不动
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func main() {
	var (
		server       = flag.String("server", envOr("YUZU_SERVER", "http://127.0.0.1:8080"), "server base URL")
		roomID       = flag.String("room", envOr("YUZU_ROOM", ""), "room id to join (required)")
		name         = flag.String("name", envOr("YUZU_NAME", "mpv-agent"), "display name")
		password     = flag.String("password", envOr("YUZU_PASSWORD", ""), "global admin password (optional)")
		roomPassword = flag.String("room-password", envOr("YUZU_ROOM_PASSWORD", ""), "room guest password")
		mpvPath      = flag.String("mpv", "mpv", "mpv binary path")
		socket       = flag.String("socket", filepath.Join(os.TempDir(), "yuzu-agent.sock"), "mpv IPC socket path")
		ao           = flag.String("ao", "", "mpv audio output (e.g. null for headless testing)")
	)
	flag.Parse()
	if *roomID == "" {
		log.Fatal("-room required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *server, *roomID, *name, *password, *roomPassword, *mpvPath, *socket, *ao); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, server, roomID, name, password, roomPassword, mpvPath, socket, ao string) error {
	// MPV 只启动一次，跨重连存活。重连成功后服务器会推送播放快照，
	// 代理把快照重新渲染进去（loadedRef 每会话重置，强制刷新）。
	os.Remove(socket)
	mpv, err := startMPV(ctx, mpvPath, socket, ao)
	if err != nil {
		return err
	}
	defer mpv.Kill()
	log.Printf("mpv started (ipc: %s)", socket)

	// 断线重连（spec-v1 §9.4）：指数退避 1s→30s；会话存活超 10s 视
	// 为连接曾稳定，退避重置。
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		start := time.Now()
		err := session(ctx, server, roomID, name, password, roomPassword, mpv)
		if ctx.Err() != nil {
			return nil // 主动退出
		}
		if time.Since(start) > 10*time.Second {
			backoff = time.Second
		}
		log.Printf("session ended: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// session 一次连接的完整生命周期：连接 → 校时 → 认证 → 进房 → 渲染。
// 任何环节失败都返回错误，由 run 决定重连。
func session(ctx context.Context, server, roomID, name, password, roomPassword string, mpv *mpvClient) error {
	// 1. 协议连接：校时 → 认证 → 进房
	cli, err := client.Dial(ctx, server)
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := cli.ClockSync(ctx, 5); err != nil {
		return err
	}
	id, err := cli.Auth(ctx, name, password)
	if err != nil {
		return err
	}
	if err := cli.Join(ctx, roomID, roomPassword); err != nil {
		return err
	}
	log.Printf("joined room %q as %s (%s)", roomID, id.Name, id.ID)

	// 3. 状态渲染 + 周期校偏
	var (
		cur       client.Playback // 最近一次服务器播放状态
		loadedRef string          // 当前已 loadfile 的 track
	)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// 防抖：快速连续的 playback.changed（如管理员连切）塌缩成最后一次 apply。
	// 否则每次 skip 都会触发一次完整的 loadfile + 探测下载 + 校偏，
	// 除了最后一首其余全是浪费。
	const debounceWindow = 400 * time.Millisecond
	applyCh := make(chan client.Playback, 1)
	var debounce *time.Timer
	syncer := &driftSyncer{}
	scheduleApply := func(pb client.Playback) {
		if debounce != nil {
			debounce.Stop()
		}
		debounce = time.AfterFunc(debounceWindow, func() {
			select {
			case applyCh <- pb:
			default:
			}
		})
	}

	apply := func(pb client.Playback) {
		cur = pb
		if pb.Current == nil {
			loadedRef = ""
			syncer.reset()
			mpv.Stop()
			return
		}
		if pb.Current.TrackRef != loadedRef {
			if err := mpv.LoadFile(server + pb.Current.StreamURL); err != nil {
				log.Printf("loadfile: %v", err)
				return
			}
			loadedRef = pb.Current.TrackRef
			syncer.reset()
			mpv.SetSpeed(1.0) // 上一首可能残留变速
			log.Printf("playing %s (%s)", pb.Current.Title, pb.Current.TrackRef)
		}
		mpv.SetPause(!pb.Playing)
		correct(mpv, cli, pb, syncer)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-cli.Events():
			if !ok {
				return fmt.Errorf("connection lost")
			}
			if m.Type == "playback.changed" {
				pb, err := client.ParsePlayback(m)
				if err == nil {
					scheduleApply(pb)
				}
			}
		case pb := <-applyCh:
			apply(pb)
		case <-ticker.C:
			if cur.Current != nil && cur.Playing {
				correct(mpv, cli, cur, syncer)
			}
		}
	}
}

// driftSyncer 区分真实漂移与恒定测量偏差。
//
// 背景：mpv 的 time-pos 读数 = 真实位置 - 音频输出延迟补偿。输出链路
// 有较大缓冲（蓝牙、大 quantum 的声卡）时，读数持续少报 ~300ms——
// 这不是漂移，拽它只会让播放每秒跳一次。判据：连续 3 次采样漂移
// 稳定（±60ms）即视为输出延迟偏差，停止纠正；漂移在变化才是真漂移。
type driftSyncer struct {
	hist   [3]int64
	n      int
	biased bool
}

func (s *driftSyncer) reset() { s.n = 0; s.biased = false }

// observe 记录一次漂移采样，返回是否已稳定（应停止纠正）。
func (s *driftSyncer) observe(drift int64) bool {
	if s.biased {
		// 已判定偏差；漂移重新大幅变化（如 seek 后未生效）时解除
		if drift > s.hist[2]+150 || drift < s.hist[2]-150 {
			s.reset()
		}
		return s.biased
	}
	if s.n < len(s.hist) {
		s.hist[s.n] = drift
		s.n++
		return false
	}
	copy(s.hist[:], s.hist[1:])
	s.hist[2] = drift
	lo, hi := s.hist[0], s.hist[0]
	for _, d := range s.hist[1:] {
		if d < lo {
			lo = d
		}
		if d > hi {
			hi = d
		}
	}
	if hi-lo <= 60 {
		s.biased = true
	}
	return s.biased
}

// correct 按漂移量分级纠正。
func correct(mpv *mpvClient, cli *client.Client, pb client.Playback, syncer *driftSyncer) {
	if pb.Current == nil {
		return
	}
	shouldMs := pb.ShouldBeMs(cli.ServerNow())
	actualMs, err := mpv.TimePos()
	if err != nil {
		return // 文件刚加载，time-pos 暂不可用
	}
	drift := actualMs - shouldMs
	if syncer.observe(drift) {
		mpv.SetSpeed(1.0)
		return
	}
	const (
		seekThreshold  = 150
		speedThreshold = 30
	)
	switch {
	case drift > seekThreshold || drift < -seekThreshold:
		log.Printf("drift %dms, seeking to %dms", drift, shouldMs)
		mpv.SeekTo(float64(shouldMs) / 1000)
		mpv.SetSpeed(1.0)
	case drift > speedThreshold:
		mpv.SetSpeed(0.98) // 本地超前，放慢
	case drift < -speedThreshold:
		mpv.SetSpeed(1.02) // 本地落后，加快
	default:
		mpv.SetSpeed(1.0)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
