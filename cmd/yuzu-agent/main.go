// Command yuzu-agent 是 MPV 播放代理：常驻进程，加入房间后把
// 服务器权威播放状态渲染到本地 MPV。纯收听，不提供控制能力。
//
// 同步策略（spec-v1 §2.2）：
//
//	should_be < 0    → 起播提前量窗口：装载并暂停待命，到点解除暂停
//	|漂移| > 150ms  → 直接 seek
//	// 30–150ms        → 调 speed（0.98–1.02）缓追  // REMOVED: 变速影响听感，见 spec-v1 讨论。纯 seek 方案
//	< 30ms          → 不动
package main

import (
	"context"
	"encoding/json"
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

// agentVersion 上报给管理面的版本号。
const agentVersion = "1.0.0"

func main() {
	var (
		server  = flag.String("server", envOr("YUZU_SERVER", "http://127.0.0.1:8080"), "server base URL")
		key     = flag.String("key", envOr("YUZU_PLAYER_KEY", ""), "persistent Player key (required)")
		mpvPath = flag.String("mpv", "mpv", "mpv binary path")
		socket  = flag.String("socket", filepath.Join(os.TempDir(), "yuzu-agent.sock"), "mpv IPC socket path")
		ao      = flag.String("ao", "", "mpv audio output (e.g. null for headless testing)")
	)
	flag.Parse()
	if *key == "" {
		log.Fatal("-key required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *server, *key, *mpvPath, *socket, *ao); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, server, playerKey, mpvPath, socket, ao string) error {
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
		err := session(ctx, server, playerKey, mpv)
		mpv.Stop()
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

// session 一次连接的完整生命周期：连接 → 校时 → Player 认证 → 服务端分配 → 渲染。
// 任何环节失败都返回错误，由 run 决定重连。
func session(ctx context.Context, server, playerKey string, mpv *mpvClient) error {
	// 1. 协议连接：校时 → Player Key 认证 → 上报设备能力
	cli, err := client.Dial(ctx, server)
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := cli.ClockSync(ctx, 5); err != nil {
		return err
	}
	identity, err := cli.AuthPlayer(ctx, playerKey)
	if err != nil {
		return err
	}
	device := "yuzu-agent"
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		device = hostname
	}
	playerID, err := cli.PlayerHello(ctx, device, agentVersion, []string{"volume", "mute"})
	if err != nil {
		return err
	}
	log.Printf("registered as player %s (%s); waiting for server Room assignment", playerID, identity.Name)
	reportState := func() {
		vol, verr := mpv.Volume()
		muted, merr := mpv.Muted()
		if verr == nil && merr == nil {
			_ = cli.SendPlayerState(int(vol+0.5), muted)
		}
	}
	reportState()

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
	//
	// 前沿触发：静默期后的第一次变更立即生效——常见的单次切歌白等一个窗口
	// 就是一个窗口的头部损失，且会挤占服务端给的起播提前量。只有窗口内
	// 后续的变更才合并到尾部，连切仍然只落地最后一首（代价是多一次 loadfile）。
	const debounceWindow = 400 * time.Millisecond
	applyCh := make(chan client.Playback, 1)
	startCh := make(chan struct{}, 1)
	var debounce *time.Timer
	var startTimer *time.Timer
	var lastApply time.Time
	syncer := &driftSyncer{}

	// cancelStart 撤销待命中的预定起播，防止旧定时器在新状态上开声。
	cancelStart := func() {
		if startTimer != nil {
			startTimer.Stop()
			startTimer = nil
		}
		select {
		case <-startCh:
		default:
		}
	}

	apply := func(pb client.Playback) {
		lastApply = time.Now()
		cancelStart()
		cur = pb
		if pb.Current == nil {
			loadedRef = ""
			syncer.reset()
			mpv.Stop()
			return
		}
		shouldMs := pb.ShouldBeMs(cli.ServerNow())
		leadIn := shouldMs < 0
		if leadIn {
			// 先暂停再装载：pause 是全局属性、跨 loadfile 保持，
			// 否则 loadfile 与 SetPause 之间会漏出开头几毫秒。
			mpv.SetPause(true)
		}
		if pb.Current.TrackRef != loadedRef {
			// 开播即定位到房间当前进度，避免从 0 播一秒再大跳。
			// 提前量窗口内（leadIn）曲目尚未开始，从头装载。
			startSec := float64(shouldMs) / 1000
			if startSec < 0 {
				startSec = 0
			}
			if err := mpv.LoadFile(server+pb.Current.StreamURL, startSec); err != nil {
				log.Printf("loadfile: %v", err)
				return
			}
			loadedRef = pb.Current.TrackRef
			syncer.reset()
			mpv.SetSpeed(1.0) // 上一首可能残留变速
			log.Printf("playing %s (%s)", pb.Current.Title, pb.Current.TrackRef)
		}
		if leadIn {
			// 起播提前量（spec-v1 §2.2）：装载完成后保持暂停占住这段窗口，
			// 到 position 0 时刻解除暂停——头部不再被装载延迟吃掉。
			// playing=false 时不排定，等服务端 resume 推新状态重新计时。
			if pb.Playing {
				startTimer = time.AfterFunc(time.Duration(-shouldMs)*time.Millisecond, func() {
					select {
					case startCh <- struct{}{}:
					default:
					}
				})
				log.Printf("lead-in: starting in %dms", -shouldMs)
			}
			return
		}
		mpv.SetPause(!pb.Playing)
		correct(mpv, cli, pb, syncer)
	}

	// scheduleApply 前沿触发 + 尾部合并，见 debounceWindow 说明。
	scheduleApply := func(pb client.Playback) {
		if debounce != nil {
			debounce.Stop()
			debounce = nil
		}
		if wait := debounceWindow - time.Since(lastApply); wait > 0 {
			debounce = time.AfterFunc(wait, func() {
				select {
				case applyCh <- pb:
				default:
				}
			})
			return
		}
		apply(pb) // 与主循环同一 goroutine，直接落地
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-cli.Events():
			if !ok {
				return fmt.Errorf("connection lost")
			}
			switch m.Type {
			case "playback.changed":
				pb, err := client.ParsePlayback(m)
				if err == nil {
					scheduleApply(pb)
				}
			case "room.left":
				if debounce != nil {
					debounce.Stop()
					debounce = nil
				}
				cancelStart()
				cur = client.Playback{}
				loadedRef = ""
				syncer.reset()
				mpv.Stop()
				log.Printf("Room assignment removed")
			case "player.command":
				op, value, err := client.ParsePlayerCommand(m)
				if err != nil {
					break
				}
				switch op {
				case "set_volume":
					var v float64
					if json.Unmarshal(value, &v) == nil {
						mpv.SetVolume(v)
						log.Printf("player.command: volume -> %.0f", v)
					}
				case "set_mute":
					var v bool
					if json.Unmarshal(value, &v) == nil {
						mpv.SetMute(v)
						log.Printf("player.command: mute -> %v", v)
					}
				}
				reportState()
			}
		case pb := <-applyCh:
			apply(pb)
		case <-startCh:
			// 预定起播时刻到达。基线从这一刻的读数重新学起。
			startTimer = nil
			if cur.Current == nil || !cur.Playing {
				break
			}
			mpv.SetPause(false)
			syncer.reset()
		case <-ticker.C:
			// 提前量窗口内曲目还没开声，不参与校偏（应播位置为负）。
			if cur.Current != nil && cur.Playing && cur.ShouldBeMs(cli.ServerNow()) >= 0 {
				correct(mpv, cli, cur, syncer)
			}
		}
	}
}

// driftSyncer 用基线学习区分真实漂移与测量偏差。
//
// 背景：mpv 的 time-pos 读数 = 真实位置 - 音频输出延迟补偿（蓝牙等
// 输出链路典型 200-300ms）。播放速率是 1:1 时，seek 生效后的第一个
// 漂移样本就是这个偏差本身——直接学为基线，后续只纠正超出基线的
// 变化，而不是反复拽一个恒定的读数差。
type driftSyncer struct {
	baseline    int64 // 已学习的测量偏差
	hasBaseline bool  // 基线是否已建立
	awaitSample bool  // 刚执行过 seek，下一个样本用于学习基线
}

func (s *driftSyncer) reset() { s.hasBaseline, s.awaitSample = false, false }

const (
	seekThreshold = 150 // 超过即 seek（校准后同样适用）
	// speedThreshold = 30  // REMOVED: 变速影响听感，不再使用。纯 seek 方案
)

// correct 按漂移量纠正；未校准时先做一次绝对对齐再学基线。
// 仅 seek，不调速（变速影响听感）。
func correct(mpv *mpvClient, cli *client.Client, pb client.Playback, syncer *driftSyncer) {
	if pb.Current == nil {
		return
	}
	shouldMs := pb.ShouldBeMs(cli.ServerNow())
	if shouldMs < 0 {
		return // 起播提前量窗口内，曲目还没开始，无从校偏
	}
	actualMs, err := mpv.TimePos()
	if err != nil {
		return // 文件刚加载，time-pos 暂不可用
	}
	drift := actualMs - shouldMs

	// 校准阶段：一次绝对对齐（或小漂移直接以当前值为基线），收敛于
	// 一两个样本，不再像稳定性判定那样连 seek 三四秒。
	if !syncer.hasBaseline {
		if syncer.awaitSample {
			syncer.baseline = drift
			syncer.hasBaseline = true
			syncer.awaitSample = false
			log.Printf("calibrated: output latency bias %dms", drift)
			return
		}
		if drift > seekThreshold || drift < -seekThreshold {
			log.Printf("drift %dms, seeking to %dms", drift, shouldMs)
			mpv.SeekTo(float64(shouldMs) / 1000)
			mpv.SetSpeed(1.0)
			syncer.awaitSample = true
		} else {
			syncer.baseline = drift
			syncer.hasBaseline = true
		}
		return
	}

	// 校准后：只纠正超出基线的变化。seek 后基线作废重新学习，
	// 输出设备延迟中途变化（蓝牙重连等）也能自适应。
	corrected := drift - syncer.baseline
	switch {
	case corrected > seekThreshold || corrected < -seekThreshold:
		log.Printf("drift %dms (bias %dms), seeking to %dms", corrected, syncer.baseline, shouldMs)
		mpv.SeekTo(float64(shouldMs) / 1000)
		mpv.SetSpeed(1.0)
		syncer.reset()
		syncer.awaitSample = true
	default:
		mpv.SetSpeed(1.0)
		// REMOVED: 变速缓追分支（corrected > speedThreshold → 0.98, < -speedThreshold → 1.02）。
		// 变速影响听感，改为仅 seek 方案。偏差未达 seekThreshold 时保持正常速度。
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
