package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIdentityBindCodeUsesCachedOIDCSessionAndPrintsExpiry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const sessionToken = "cached-oidc-session-token"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.RequestURI != "/api/v1/auth/external-binding-codes" {
			t.Errorf("request = %s %s", r.Method, r.RequestURI)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+sessionToken {
			t.Errorf("Authorization = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if len(body) != 0 {
			t.Errorf("body = %q, want empty", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"code":"7K3M-9P2D-X4RT","expires_at":1720000600000}`)
	}))
	defer api.Close()

	if err := saveSession(cachedSession{Server: api.URL, Token: sessionToken, Kind: "oidc"}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := issueIdentityBindCode(context.Background(), api.URL, &output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"code: 7K3M-9P2D-X4RT", "expires_at: 1720000600000", "到期时间:"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, sessionToken) {
		t.Fatalf("output exposed session token: %q", got)
	}
}

func TestIdentityBindCodeCommandRejectsNonOIDCSessionWithoutRequest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	requests := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Error("binding command made a request with a non-OIDC session")
	}))
	defer api.Close()

	if err := saveSession(cachedSession{Server: api.URL, Token: "guest-session-token", Kind: "guest"}); err != nil {
		t.Fatal(err)
	}
	previousServer := *server
	*server = api.URL
	t.Cleanup(func() { *server = previousServer })

	err := commands["identity bind-code"].run(nil)
	if err == nil || !strings.Contains(err.Error(), "yuzu-cli login") {
		t.Fatalf("error = %v, want explicit yuzu-cli login guidance", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestIdentityBindCodeRejectsSessionForDifferentServerWithoutRequest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	requests := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Error("binding command used a session cached for another server")
	}))
	defer api.Close()

	if err := saveSession(cachedSession{Server: "https://other.example", Token: "oidc-token", Kind: "oidc"}); err != nil {
		t.Fatal(err)
	}
	err := issueIdentityBindCode(context.Background(), api.URL, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "yuzu-cli login") {
		t.Fatalf("error = %v, want explicit yuzu-cli login guidance", err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}
