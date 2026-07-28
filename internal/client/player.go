package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/coder/websocket"
)

// ---------- 播放端管理平面 ----------

// PlayerHello 用稳定 player_id 注册为可管理播放端。
// caps 建议：["volume","mute","join_room"]。
func (c *Client) PlayerHello(
	ctx context.Context,
	playerID, device, version string,
	caps []string,
) (string, error) {
	m, err := c.call(ctx, "player.hello", map[string]any{
		"player_id": playerID, "device": device, "version": version, "caps": caps,
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

// PlayerInfo 播放端快照（REST）。
type PlayerInfo struct {
	ID          string   `json:"id"`
	Device      string   `json:"device"`
	Version     string   `json:"version,omitempty"`
	Caps        []string `json:"caps"`
	IdentityID  string   `json:"identity_id"`
	Identity    string   `json:"identity_name"`
	RoomID      string   `json:"room_id,omitempty"`
	Volume      int      `json:"volume"`
	Muted       bool     `json:"muted"`
	ConnectedAt int64    `json:"connected_at"`
}

// RESTPlayers 在线播放端清单（room_admin）。
func RESTPlayers(ctx context.Context, server, token string) ([]PlayerInfo, error) {
	var out struct {
		Players []PlayerInfo `json:"players"`
	}
	err := restCall(ctx, server, http.MethodGet, "/api/v1/players", token, nil, &out)
	return out.Players, err
}

// RESTPlayerCommand 向播放端下发指令（room_admin）。
// op: set_volume(int) | set_mute(bool) | join_room(room_id string)。
func RESTPlayerCommand(ctx context.Context, server, token, playerID, op string, value any) error {
	return restCall(ctx, server, http.MethodPost, "/api/v1/players/"+url.PathEscape(playerID)+"/command", token,
		map[string]any{"op": op, "value": value}, &struct{}{})
}

// RoomPlayerInfo 是 Room primary player 的在线状态。
type RoomPlayerInfo struct {
	ID       string `json:"id"`
	Online   bool   `json:"online"`
	Device   string `json:"device,omitempty"`
	RoomID   string `json:"room_id,omitempty"`
	Volume   int    `json:"volume"`
	Muted    bool   `json:"muted"`
	Identity string `json:"identity_name,omitempty"`
}

// RESTRoomPlayer 读取 Room 当前绑定的 primary player。
// Room controller 和允许成员调音量的同 Room Integration actor 可调用。
func RESTRoomPlayer(ctx context.Context, server, token, roomID string) (RoomPlayerInfo, error) {
	var out struct {
		Player RoomPlayerInfo `json:"player"`
	}
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/player", token, nil, &out)
	return out.Player, err
}

// RESTBindRoomPlayer 由 room_admin 绑定在线 player，并将其迁移到目标 Room。
func RESTBindRoomPlayer(
	ctx context.Context,
	server, token, roomID, playerID string,
) (RoomPlayerInfo, error) {
	var out struct {
		Player RoomPlayerInfo `json:"player"`
	}
	err := restCall(ctx, server, http.MethodPut,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/player", token,
		map[string]any{"player_id": playerID}, &out)
	return out.Player, err
}

// RESTUnbindRoomPlayer 由 room_admin 解除 Room primary player 绑定。
func RESTUnbindRoomPlayer(ctx context.Context, server, token, roomID string) error {
	return restCall(ctx, server, http.MethodDelete,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/player", token, nil, &struct{}{})
}

// RESTRoomPlayerSetVolume 调整 Room primary player 音量。
// Integration actor 调用时，ctx 必须通过 WithIdempotencyKey 携带平台事件 ID。
func RESTRoomPlayerSetVolume(
	ctx context.Context,
	server, actorToken, roomID string,
	volume int,
) error {
	return restCall(ctx, server, http.MethodPost,
		"/api/v1/rooms/"+url.PathEscape(roomID)+"/player/volume", actorToken,
		map[string]any{"volume": volume}, &struct{}{})
}
