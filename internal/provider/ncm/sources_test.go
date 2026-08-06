package ncm

import (
	"reflect"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
)

func TestRadioSources(t *testing.T) {
	p := &Provider{}
	got := p.RadioSources()
	tests := []struct {
		name   string
		want   provider.RadioSource
		source provider.TrackSource
	}{
		{
			name:   "daily",
			want:   provider.RadioSource{Spec: "daily", Name: "每日推荐", Finite: true},
			source: &dailySource{p: p},
		},
		{
			name:   "fm",
			want:   provider.RadioSource{Spec: "fm", Name: "私人 FM", Finite: false},
			source: &fmSource{p: p},
		},
		{
			name:   "simi",
			want:   provider.RadioSource{Spec: "simi", Arg: "track_id", Name: "相似歌曲", Finite: false},
			source: &chainedSource{p: p, kind: "simi"},
		},
		{
			name:   "heart",
			want:   provider.RadioSource{Spec: "heart", Arg: "track_id", Name: "心动模式", Finite: false},
			source: &chainedSource{p: p, kind: "heart"},
		},
	}
	if len(got) != len(tests) {
		t.Fatalf("RadioSources() returned %d entries, want %d: %#v", len(got), len(tests), got)
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(got[i], tt.want) {
				t.Fatalf("RadioSources()[%d] = %#v, want %#v", i, got[i], tt.want)
			}
			if got[i].Finite != tt.source.Finite() {
				t.Fatalf("catalog Finite = %v, source Finite() = %v", got[i].Finite, tt.source.Finite())
			}
		})
	}
}
