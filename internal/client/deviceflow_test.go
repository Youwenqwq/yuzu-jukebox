package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDeviceIdP 可编程的设备流 IdP。
type fakeDeviceIdP struct {
	srv        *httptest.Server
	polls      atomic.Int32
	pendingN   int32  // 前 N 次轮询返回 authorization_pending
	finalError string // 非空则最终返回该 OAuth error；空则成功
}

func newFakeDeviceIdP(t *testing.T, pendingN int32, finalError string) *fakeDeviceIdP {
	t.Helper()
	f := &fakeDeviceIdP{pendingN: pendingN, finalError: finalError}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v2/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("client_id") == "" {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dev-1", "user_code": "ABCD-EFGH",
			"verification_uri": "https://idp.example/device",
			"expires_in":       300, "interval": 1, // 测试里等不起更久
		})
	})
	mux.HandleFunc("/oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": "unsupported_grant_type"})
			return
		}
		if f.polls.Add(1) <= f.pendingN {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		if f.finalError != "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": f.finalError})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id_token": "header.payload.sig"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestDeviceFlowPendingThenSuccess(t *testing.T) {
	f := newFakeDeviceIdP(t, 2, "")
	var shown string
	tok, _, err := DeviceFlowLogin(context.Background(), f.srv.URL, "cli-1",
		func(uri, code string) { shown = uri + "|" + code })
	if err != nil {
		t.Fatal(err)
	}
	if tok != "header.payload.sig" {
		t.Fatalf("unexpected token %q", tok)
	}
	if shown != "https://idp.example/device|ABCD-EFGH" {
		t.Fatalf("display got %q", shown)
	}
	if f.polls.Load() != 3 {
		t.Fatalf("want 3 polls, got %d", f.polls.Load())
	}
}

func TestDeviceFlowDenied(t *testing.T) {
	f := newFakeDeviceIdP(t, 0, "access_denied")
	_, _, err := DeviceFlowLogin(context.Background(), f.srv.URL, "cli-1", func(string, string) {})
	if !errors.Is(err, ErrDeviceDenied) {
		t.Fatalf("want ErrDeviceDenied, got %v", err)
	}
}

func TestDeviceFlowExpired(t *testing.T) {
	f := newFakeDeviceIdP(t, 0, "expired_token")
	_, _, err := DeviceFlowLogin(context.Background(), f.srv.URL, "cli-1", func(string, string) {})
	if !errors.Is(err, ErrDeviceExpired) {
		t.Fatalf("want ErrDeviceExpired, got %v", err)
	}
}

func TestDeviceFlowContextCancel(t *testing.T) {
	f := newFakeDeviceIdP(t, 1<<30, "") // 永远 pending
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_, _, err := DeviceFlowLogin(ctx, f.srv.URL, "cli-1", func(string, string) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want ctx deadline, got %v", err)
	}
}
