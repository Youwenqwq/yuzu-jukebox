package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
)

func TestGuestAuthPasswordProbeRateLimit(t *testing.T) {
	s := &Server{authm: auth.NewManager("", nil)}

	for i := range 10 {
		response := performGuestAuth(t, s, "disabled-password-probe", fmt.Sprintf("203.0.113.77:%d", 3000+i))
		if response.Code != http.StatusOK {
			t.Fatalf("probe %d status = %d, want %d", i+1, response.Code, http.StatusOK)
		}
	}

	response := performGuestAuth(t, s, "disabled-password-probe", "203.0.113.77:4000")
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("limited status = %d, want %d", response.Code, http.StatusTooManyRequests)
	}
	var got struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode rate-limit response: %v", err)
	}
	if got.Error.Code != "rate_limited" || got.Error.Message != auth.ErrPasswordProbeRateLimited.Error() {
		t.Fatalf("rate-limit error = %#v", got.Error)
	}

	response = performGuestAuth(t, s, "", "203.0.113.77:5000")
	if response.Code != http.StatusOK {
		t.Fatalf("passwordless guest status = %d, want %d", response.Code, http.StatusOK)
	}
	response = performGuestAuth(t, s, "disabled-password-probe", "198.51.100.22:3000")
	if response.Code != http.StatusOK {
		t.Fatalf("independent IP status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestGuestAuthPasswordlessRateLimit(t *testing.T) {
	s := &Server{authm: auth.NewManager("", nil)}
	for i := range 20 {
		response := performGuestAuth(t, s, "", fmt.Sprintf("192.0.2.10:%d", 3000+i))
		if response.Code != http.StatusOK {
			t.Fatalf("guest login %d status = %d, want %d", i+1, response.Code, http.StatusOK)
		}
	}
	response := performGuestAuth(t, s, "", "192.0.2.10:4000")
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("limited status = %d, want %d", response.Code, http.StatusTooManyRequests)
	}
	var got struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode rate-limit response: %v", err)
	}
	if got.Error.Code != "rate_limited" || got.Error.Message != auth.ErrGuestAuthRateLimited.Error() {
		t.Fatalf("rate-limit error = %#v", got.Error)
	}
}

func TestGuestAuthNameByteLimit(t *testing.T) {
	s := &Server{authm: auth.NewManager("", nil)}
	if response := performGuestAuthName(t, s, strings.Repeat("a", 64), "", "198.51.100.1:3000"); response.Code != http.StatusOK {
		t.Fatalf("64-byte name status = %d, want %d", response.Code, http.StatusOK)
	}
	if response := performGuestAuthName(t, s, strings.Repeat("界", 22), "", "198.51.100.2:3000"); response.Code != http.StatusBadRequest {
		t.Fatalf("66-byte name status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestHandlerLimitsRequestBodies(t *testing.T) {
	s := &Server{authm: auth.NewManager("secret", nil)}
	body, err := json.Marshal(map[string]string{
		"name": "visitor", "password": strings.Repeat("x", (1<<20)+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/guest", bytes.NewReader(body))
	r.RemoteAddr = "198.51.100.3:3000"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func performGuestAuth(t *testing.T, s *Server, password, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	return performGuestAuthName(t, s, "visitor", password, remoteAddr)
}

func performGuestAuthName(t *testing.T, s *Server, name, password, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/guest", bytes.NewReader(body))
	r.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	s.guestAuth(w, r)
	return w
}
