package wsapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
)

func TestGuestAuthPasswordProbeRateLimit(t *testing.T) {
	authm := auth.NewManager("", nil)
	for i := range 10 {
		response := dispatchGuestAuth(t, authm, "disabled-password-probe", fmt.Sprintf("203.0.113.55:%d", 6000+i))
		if response["type"] != "auth.ok" {
			t.Fatalf("probe %d response type = %v, want auth.ok", i+1, response["type"])
		}
	}

	response := dispatchGuestAuth(t, authm, "disabled-password-probe", "203.0.113.55:7000")
	if response["type"] != "error" {
		t.Fatalf("limited response type = %v, want error", response["type"])
	}
	data, ok := response["data"].(map[string]any)
	if !ok {
		t.Fatalf("limited response data = %#v", response["data"])
	}
	if data["code"] != "rate_limited" || data["message"] != auth.ErrPasswordProbeRateLimited.Error() {
		t.Fatalf("limited response data = %#v", data)
	}

	response = dispatchGuestAuth(t, authm, "", "203.0.113.55:8000")
	if response["type"] != "auth.ok" {
		t.Fatalf("passwordless response type = %v, want auth.ok", response["type"])
	}
}

func TestGuestAuthNameValidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		guest    string
		wantType string
		wantCode string
	}{
		{name: "70 bytes rejected", guest: strings.Repeat("a", 70), wantType: "error", wantCode: "bad_request"},
		{name: "control character rejected", guest: "line\nbreak", wantType: "error", wantCode: "bad_request"},
		{name: "64 bytes accepted", guest: strings.Repeat("a", 64), wantType: "auth.ok"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dispatchGuestAuthName(t, auth.NewManager("", nil), test.guest, "", "198.51.100.8:6000")
			if response["type"] != test.wantType {
				t.Fatalf("response type = %v, want %s", response["type"], test.wantType)
			}
			if test.wantCode == "" {
				return
			}
			data, ok := response["data"].(map[string]any)
			if !ok {
				t.Fatalf("response data = %#v", response["data"])
			}
			if data["code"] != test.wantCode {
				t.Fatalf("response code = %v, want %s", data["code"], test.wantCode)
			}
		})
	}
}

func dispatchGuestAuth(t *testing.T, authm *auth.Manager, password, remoteAddr string) map[string]any {
	t.Helper()
	return dispatchGuestAuthName(t, authm, "visitor", password, remoteAddr)
}

func dispatchGuestAuthName(t *testing.T, authm *auth.Manager, name, password, remoteAddr string) map[string]any {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"name": name, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	c := &client{
		server: &Server{authm: authm},
		remote: remoteAddr,
		send:   make(chan any, 1),
	}
	c.dispatch("auth", "probe", payload)
	response, ok := (<-c.send).(map[string]any)
	if !ok {
		t.Fatalf("response = %#v", response)
	}
	return response
}
