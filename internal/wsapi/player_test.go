package wsapi

import (
	"encoding/json"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
)

func TestPlayerStateMetricsSnapshot(t *testing.T) {
	c := &client{
		server:   &Server{players: make(map[string]*client)},
		identity: &auth.Identity{PlayerID: "player-1"},
		player:   &playerState{id: "player-1"},
	}
	c.server.players[c.player.id] = c

	for _, step := range []struct {
		name    string
		payload string
		want    map[string]any
		absent  []string
	}{
		{
			name:    "旧播放端不暴露遥测字段",
			payload: `{"volume":35,"muted":true}`,
			want:    map[string]any{"volume": float64(35), "muted": true},
			absent:  []string{"drift_ms", "baseline_ms", "load_ms"},
		},
		{
			name:    "完整遥测进入快照",
			payload: `{"volume":40,"muted":false,"drift_ms":-21,"baseline_ms":85,"load_ms":340}`,
			want:    map[string]any{"volume": float64(40), "muted": false, "drift_ms": float64(-21), "baseline_ms": float64(85), "load_ms": float64(340)},
		},
		{
			name:    "独立遥测保留音量和未上报指标",
			payload: `{"load_ms":210}`,
			want:    map[string]any{"volume": float64(40), "muted": false, "drift_ms": float64(-21), "baseline_ms": float64(85), "load_ms": float64(210)},
		},
		{
			name:    "显式零值清除旧指标",
			payload: `{"drift_ms":0}`,
			want:    map[string]any{"baseline_ms": float64(85), "load_ms": float64(210)},
			absent:  []string{"drift_ms"},
		},
		{
			name:    "无效指标拒绝整条上报",
			payload: `{"volume":99,"baseline_ms":"invalid"}`,
			want:    map[string]any{"volume": float64(40), "baseline_ms": float64(85), "load_ms": float64(210)},
		},
	} {
		t.Run(step.name, func(t *testing.T) {
			c.dispatch("player.state", "", json.RawMessage(step.payload))
			info, err := c.server.Player(c.player.id)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(info)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			for key, want := range step.want {
				if got[key] != want {
					t.Errorf("%s = %v, want %v", key, got[key], want)
				}
			}
			for _, key := range step.absent {
				if _, ok := got[key]; ok {
					t.Errorf("unexpected %s in snapshot: %s", key, encoded)
				}
			}
		})
	}
}
