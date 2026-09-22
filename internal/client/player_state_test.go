package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestPlayerStateWireCompatibility(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frames := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for range 3 {
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			frames <- string(data)
		}
	}))
	defer server.Close()
	cli, err := Dial(ctx, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	for _, tc := range []struct {
		report PlayerStateReport
		want   string
	}{
		{
			report: PlayerStateReport{Volume: 37, Muted: true, DriftMs: -42, BaselineMs: -200, LoadMs: 80},
			want:   `{"data":{"muted":true,"volume":37},"type":"player.state"}`,
		},
		{
			report: PlayerStateReport{Volume: 37, Muted: true, DriftMs: -42, BaselineMs: -200, LoadMs: 80, HasMetrics: true},
			want:   `{"data":{"baseline_ms":-200,"drift_ms":-42,"load_ms":80,"muted":true,"volume":37},"type":"player.state"}`,
		},
		{
			report: PlayerStateReport{HasMetrics: true},
			want:   `{"data":{"baseline_ms":0,"drift_ms":0,"load_ms":0,"muted":false,"volume":0},"type":"player.state"}`,
		},
	} {
		if err := cli.SendPlayerState(tc.report); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-frames:
			if got != tc.want {
				t.Errorf("frame = %s; want %s", got, tc.want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestClockSyncConcurrentWithEventsAndServerNow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var request Message
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"playback.changed","data":{}}`)); err != nil {
				return
			}
			pong, err := json.Marshal(map[string]any{
				"type": "pong", "ref": request.Ref,
				"data": map[string]any{"server_time": time.Now().UnixMilli() + 10000},
			})
			if err != nil {
				t.Error(err)
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, pong); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	cli, err := Dial(ctx, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	results := make(chan error, 2)
	var readers sync.WaitGroup
	readers.Add(1)
	stop := make(chan struct{})
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				cli.ServerNow()
			}
		}
	}()
	defer func() { close(stop); readers.Wait() }()
	for range 2 {
		go func() { results <- cli.ClockSync(ctx, 3) }()
	}
	for range 6 {
		select {
		case event := <-cli.Events():
			if event.Type != "playback.changed" {
				t.Fatalf("unexpected broadcast: %s", event.Type)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if offset := cli.ServerNow() - time.Now().UnixMilli(); offset < 9000 || offset > 11000 {
		t.Fatalf("clock offset = %dms, want about 10000ms", offset)
	}
}
