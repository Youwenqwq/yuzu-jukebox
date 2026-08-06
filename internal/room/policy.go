package room

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
)

var (
	ErrQueueFull     = errors.New("queue is full")
	ErrQuotaExceeded = errors.New("queue quota exceeded for your role")
	ErrInvalidPolicy = errors.New("invalid policy")
)

// Policy 房间治理策略（JSON 存于 rooms.policy_json，随房间持久化）。
//
//	{"max_queue": 100, "queue_limits": {"guest": 5, "room_admin": 0}}
//
// queue_limits 的 key 匹配身份的 kind（guest/password/oidc）或其任一 role
// （listener/requester/room_admin/media_admin）；任一命中值为 0 → 显式不限，
// 否则取命中最大值；无命中 = 不限。guest 身份 ID 由名字确定性派生，
// 同名重连仍受同一限额约束。
type Policy struct {
	MaxQueue           int            `json:"max_queue,omitempty"`            // 待播队列总上限，0 = 不限
	QueueLimits        map[string]int `json:"queue_limits,omitempty"`         // kind/role → 待播上限
	MemberPlayerVolume bool           `json:"member_player_volume,omitempty"` // scoped Integration actor 可调绑定播放器音量
	// RadioControl 控制电台启停权限："" 或 "controller" 保持仅 controller 可用；
	// "requester" 允许任何具全局 requester 角色的 Principal 操作。
	RadioControl string `json:"radio_control,omitempty"`
	// StartLeadMs 切歌起播提前量（毫秒）：新曲目的 position 0 被排在
	// 「切歌时刻 + 提前量」，客户端拿到这段窗口装载解码，到点同时开声，
	// 头部不再被装载延迟吃掉（spec-v1 §2.2 起播提前量）。
	// nil = 用 DefaultStartLeadMs；0 = 关闭（切歌即 position 0，旧行为）。
	// 房间内客户端装载普遍慢（公网/蓝牙输出）时调大。
	StartLeadMs *int `json:"start_lead_ms,omitempty"`
}

// MaxStartLeadMs 起播提前量上限。超过 5s 的曲间静默不像点唱机像故障。
const MaxStartLeadMs = 5000

// ParsePolicy 解析并校验策略 JSON。空串等价于 {}。
func ParsePolicy(raw string) (Policy, error) {
	var p Policy
	if raw == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("%w: %v", ErrInvalidPolicy, err)
	}
	if p.MaxQueue < 0 {
		return p, fmt.Errorf("%w: max_queue must be >= 0", ErrInvalidPolicy)
	}
	for k, v := range p.QueueLimits {
		if v < 0 {
			return p, fmt.Errorf("%w: queue_limits[%q] must be >= 0", ErrInvalidPolicy, k)
		}
	}
	switch p.RadioControl {
	case "", "controller", "requester":
	default:
		return p, fmt.Errorf("%w: radio_control must be one of \"controller\" or \"requester\"", ErrInvalidPolicy)
	}
	if p.StartLeadMs != nil && (*p.StartLeadMs < 0 || *p.StartLeadMs > MaxStartLeadMs) {
		return p, fmt.Errorf("%w: start_lead_ms must be within [0, %d]", ErrInvalidPolicy, MaxStartLeadMs)
	}
	return p, nil
}

// startLeadMs 本房间生效的起播提前量。
func (p Policy) startLeadMs() int64 {
	if p.StartLeadMs != nil {
		return int64(*p.StartLeadMs)
	}
	return DefaultStartLeadMs
}

// queueLimit 该身份的待播上限：0 = 不限。
// 任一命中键的值为 0 → 显式不限（直接胜出）；否则取命中值的最大值。
func (p Policy) queueLimit(id auth.Identity) int {
	limit := 0
	for _, key := range append([]string{id.Kind}, id.Roles...) {
		v, ok := p.QueueLimits[key]
		if !ok {
			continue
		}
		if v == 0 {
			return 0
		}
		if v > limit {
			limit = v
		}
	}
	return limit
}
