package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

// mpvClient 通过 MPV 的 JSON IPC（unix socket）控制播放器。
type mpvClient struct {
	cmd  *exec.Cmd
	conn net.Conn
	enc  *json.Encoder

	wmu     sync.Mutex
	pmu     sync.Mutex
	pending map[int]chan mpvResponse
	reqSeq  int
}

type mpvResponse struct {
	RequestID int             `json:"request_id"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

// startMPV 启动 mpv 并等待 IPC socket 就绪。
func startMPV(ctx context.Context, mpvPath, socket, ao string) (*mpvClient, error) {
	args := []string{
		"--idle=yes",
		"--input-ipc-server=" + socket,
		"--no-terminal",
		"--really-quiet",
	}
	if ao != "" {
		args = append(args, "--ao="+ao)
	}
	cmd := exec.CommandContext(ctx, mpvPath, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mpv: %w", err)
	}

	// 等待 socket 出现（mpv 启动有延迟）
	deadline := time.Now().Add(5 * time.Second)
	var conn net.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", socket)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("mpv ipc socket not ready: %w", err)
	}

	m := &mpvClient{cmd: cmd, conn: conn, enc: json.NewEncoder(conn), pending: map[int]chan mpvResponse{}}
	go m.readPump()
	return m, nil
}

// readPump 读 IPC 响应；mpv 的异步事件（无 request_id）直接忽略——
// 服务器是唯一权威，本地播放事件不影响状态。
func (m *mpvClient) readPump() {
	scanner := bufio.NewScanner(m.conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var resp mpvResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			continue
		}
		if resp.RequestID == 0 {
			continue
		}
		m.pmu.Lock()
		if ch, ok := m.pending[resp.RequestID]; ok {
			ch <- resp
			delete(m.pending, resp.RequestID)
		}
		m.pmu.Unlock()
	}
}

func (m *mpvClient) command(args ...any) (mpvResponse, error) {
	m.pmu.Lock()
	m.reqSeq++
	id := m.reqSeq
	ch := make(chan mpvResponse, 1)
	m.pending[id] = ch
	m.pmu.Unlock()

	msg := map[string]any{"command": args, "request_id": id}
	m.wmu.Lock()
	err := m.enc.Encode(msg)
	m.wmu.Unlock()
	if err != nil {
		return mpvResponse{}, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(3 * time.Second):
		return mpvResponse{}, fmt.Errorf("mpv command timeout")
	}
}

// LoadFile 加载 URL；startSec > 0 时用 loadfile options 开播即定位
// （mpv ≥0.34），避免从 0 播一秒再大跳。
func (m *mpvClient) LoadFile(url string, startSec float64) error {
	var resp mpvResponse
	var err error
	if startSec > 0 {
		resp, err = m.command("loadfile", url, "replace", -1,
			map[string]any{"start": fmt.Sprintf("%.3f", startSec)})
	} else {
		resp, err = m.command("loadfile", url)
	}
	if err != nil {
		return err
	}
	if resp.Error != "success" {
		return fmt.Errorf("loadfile: %s", resp.Error)
	}
	return nil
}

func (m *mpvClient) Stop() {
	m.command("stop")
}

func (m *mpvClient) SetPause(paused bool) {
	m.command("set_property", "pause", paused)
}

func (m *mpvClient) SetSpeed(speed float64) {
	m.command("set_property", "speed", speed)
}

func (m *mpvClient) SeekTo(seconds float64) {
	m.command("set_property", "time-pos", seconds)
}

// TimePos 返回当前播放位置（毫秒）。文件未加载时返回错误。
func (m *mpvClient) TimePos() (int64, error) {
	resp, err := m.command("get_property", "time-pos")
	if err != nil {
		return 0, err
	}
	if resp.Error != "success" {
		return 0, fmt.Errorf("time-pos: %s", resp.Error)
	}
	var sec float64
	if err := json.Unmarshal(resp.Data, &sec); err != nil {
		return 0, err
	}
	return int64(sec * 1000), nil
}

func (m *mpvClient) Kill() {
	m.conn.Close()
	if m.cmd.Process != nil {
		m.cmd.Process.Kill()
		m.cmd.Wait()
	}
}
