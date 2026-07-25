package client

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"
)

// ---------- 播放端管理平面 ----------

// PlayerHello 注册为可管理播放端，返回 player_id。
// caps 建议：["volume", "mute", "join_room"]（声明可调能力）。
func (c *Client) PlayerHello(ctx context.Context, device, version string, caps []string) (string, error) {
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

// PlayerInfo 播放端快照（REST）。
type PlayerInfo struct {
	ID          string   `json:"id"`
	Device      string   `json:"device"`
	Version     string   `json:"version,omitempty"`
	Caps        []string `json:"caps"`
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
	err := restCall(ctx, server, "GET", "/api/v1/players", token, nil, &out)
	return out.Players, err
}

// RESTPlayerCommand 向播放端下发指令（room_admin）。
// op: set_volume(int) | set_mute(bool) | join_room(room_id string)。
func RESTPlayerCommand(ctx context.Context, server, token, playerID, op string, value any) error {
	return restCall(ctx, server, "POST", "/api/v1/players/"+playerID+"/command", token,
		map[string]any{"op": op, "value": value}, &struct{}{})
}
