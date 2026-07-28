package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
)

// ---------- 播放端管理平面 ----------

// PlayerHello registers runtime device metadata after Player-key authentication.
func (c *Client) PlayerHello(
	ctx context.Context,
	device, version string,
	caps []string,
) (string, error) {
	m, err := c.call(ctx, "player.hello", map[string]any{
		"device": device, "version": version, "caps": caps,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		PlayerID string `json:"player_id"`
	}
	if err := json.Unmarshal(m.Data, &out); err != nil {
		return "", err
	}
	return out.PlayerID, nil
}

// SendPlayerState 上报播放端状态（fire-and-forget，无 ref）。
func (c *Client) SendPlayerState(volume int, muted bool) error {
	payload, err := json.Marshal(map[string]any{
		"type": "player.state",
		"data": map[string]any{"volume": volume, "muted": muted},
	})
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.conn.Write(context.Background(), websocket.MessageText, payload)
}

// ParsePlayerCommand 解析服务端下发的 player.command。
func ParsePlayerCommand(m Message) (op string, value json.RawMessage, err error) {
	var d struct {
		Op    string          `json:"op"`
		Value json.RawMessage `json:"value"`
	}
	err = json.Unmarshal(m.Data, &d)
	return d.Op, d.Value, err
}

// PlayerInfo is the persistent Player resource merged with online runtime state.
type PlayerInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Active        bool     `json:"active"`
	KeyConfigured bool     `json:"key_configured"`
	Online        bool     `json:"online"`
	RoomID        string   `json:"room_id,omitempty"`
	Device        string   `json:"device,omitempty"`
	Version       string   `json:"version,omitempty"`
	Caps          []string `json:"caps"`
	Volume        int      `json:"volume,omitempty"`
	Muted         bool     `json:"muted,omitempty"`
	CreatedAt     int64    `json:"created_at"`
	UpdatedAt     int64    `json:"updated_at"`
	LastSeenAt    *int64   `json:"last_seen_at,omitempty"`
	ConnectedAt   int64    `json:"connected_at,omitempty"`
}

// RESTPlayers lists persistent Players and their current online state.
func RESTPlayers(ctx context.Context, server, token string) ([]PlayerInfo, error) {
	var out struct {
		Players []PlayerInfo `json:"players"`
	}
	err := restCall(ctx, server, http.MethodGet, "/api/v1/players", token, nil, &out)
	return out.Players, err
}

type PlayerCredential struct {
	Player PlayerInfo `json:"player"`
	Key    string     `json:"key"`
}

func RESTCreatePlayer(ctx context.Context, server, token, id, name string) (PlayerCredential, error) {
	var out PlayerCredential
	err := restCall(ctx, server, http.MethodPost, "/api/v1/players", token,
		map[string]any{"id": id, "name": name}, &out)
	return out, err
}

func RESTGetPlayer(ctx context.Context, server, token, playerID string) (PlayerInfo, error) {
	var out struct {
		Player PlayerInfo `json:"player"`
	}
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/players/"+url.PathEscape(playerID), token, nil, &out)
	return out.Player, err
}

type PlayerUpdate struct {
	Name   *string `json:"name,omitempty"`
	Active *bool   `json:"active,omitempty"`
}

func RESTUpdatePlayer(
	ctx context.Context,
	server, token, playerID string,
	update PlayerUpdate,
) (PlayerInfo, error) {
	var out struct {
		Player PlayerInfo `json:"player"`
	}
	err := restCall(ctx, server, http.MethodPatch,
		"/api/v1/players/"+url.PathEscape(playerID), token, update, &out)
	return out.Player, err
}

func RESTRotatePlayerKey(ctx context.Context, server, token, playerID string) (PlayerCredential, error) {
	var out PlayerCredential
	err := restCall(ctx, server, http.MethodPost,
		"/api/v1/players/"+url.PathEscape(playerID)+"/key", token, nil, &out)
	return out, err
}

func RESTDeletePlayer(ctx context.Context, server, token, playerID string) error {
	return restCall(ctx, server, http.MethodDelete,
		"/api/v1/players/"+url.PathEscape(playerID), token, nil, &struct{}{})
}

// RESTPlayerCommand sends an online device command (room_admin).
// op: set_volume(int) | set_mute(bool).
func RESTPlayerCommand(ctx context.Context, server, token, playerID, op string, value any) error {
	return restCall(ctx, server, http.MethodPost, "/api/v1/players/"+url.PathEscape(playerID)+"/command", token,
		map[string]any{"op": op, "value": value}, &struct{}{})
}

// RoomPlayerInfo 是 Room 中 headless player 的绑定和在线状态。
type RoomPlayerInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Bound  bool   `json:"bound"`
	Online bool   `json:"online"`
	Device string `json:"device,omitempty"`
	RoomID string `json:"room_id,omitempty"`
	Volume int    `json:"volume"`
	Muted  bool   `json:"muted"`
}

// RoomOutput 是 Room 的权威 headless output desired state。
// Volume 为 nil 表示从未设置，不覆盖 Agent 的设备本地音量。
type RoomOutput struct {
	Volume    *int  `json:"volume"`
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

type RoomOutputUpdate struct {
	Output   RoomOutput `json:"output"`
	Delivery struct {
		CommandsSent int `json:"commands_sent"`
	} `json:"delivery"`
}

// RESTRoomOutput 读取 Room 的 desired output state。
func RESTRoomOutput(ctx context.Context, server, token, roomID string) (RoomOutput, error) {
	var out struct {
		Output RoomOutput `json:"output"`
	}
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/output", token, nil, &out)
	return out.Output, err
}

// RESTRoomOutputSetVolume 设置 Room 的 desired volume 并 fan-out 至当前在线
// headless players。Integration actor 调用时，ctx 必须通过
// WithIdempotencyKey 携带平台事件 ID。
func RESTRoomOutputSetVolume(
	ctx context.Context,
	server, actorToken, roomID string,
	volume int,
) (RoomOutputUpdate, error) {
	var out RoomOutputUpdate
	err := restCall(ctx, server, http.MethodPatch,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/output", actorToken,
		map[string]any{"volume": volume}, &out)
	return out, err
}

// RESTRoomPlayers 读取 Room 的绑定 player 与当前在线 headless player 并集。
func RESTRoomPlayers(ctx context.Context, server, token, roomID string) ([]RoomPlayerInfo, error) {
	var out struct {
		Players []RoomPlayerInfo `json:"players"`
	}
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/players", token, nil, &out)
	return out.Players, err
}

// RESTBindRoomPlayer persistently assigns an online or offline Player to a Room.
func RESTBindRoomPlayer(
	ctx context.Context,
	server, token, roomID, playerID string,
) (RoomPlayerInfo, error) {
	var out struct {
		Player RoomPlayerInfo `json:"player"`
	}
	err := restCall(ctx, server, http.MethodPut,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/players/"+url.PathEscape(playerID),
		token, nil, &out)
	return out.Player, err
}

// RESTUnbindRoomPlayer 由 room_admin 解除指定 player 的 Room 分配。
func RESTUnbindRoomPlayer(ctx context.Context, server, token, roomID, playerID string) error {
	return restCall(ctx, server, http.MethodDelete,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/players/"+url.PathEscape(playerID),
		token, nil, &struct{}{})
}
