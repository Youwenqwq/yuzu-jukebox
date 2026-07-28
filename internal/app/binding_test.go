package app_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
)

func TestOIDCSelfServiceExternalBindingEndToEnd(t *testing.T) {
	e := newOIDCEnv(t)
	identity, oidcToken := e.oidcLogin(t, map[string]any{
		"jukebox-admin": map[string]string{"999": "org.example"},
	})
	principalID, _ := identity["id"].(string)

	createResp := bindingRequest(t, e, http.MethodPost, "/api/v1/integrations", oidcToken, map[string]any{
		"id": "binding-bridge", "name": "Binding bridge",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create integration status = %d: %s", createResp.StatusCode, data)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	unauthenticated := bindingRequest(t, e, http.MethodPost, "/api/v1/auth/external-binding-codes", "", nil)
	assertBindingError(t, unauthenticated, http.StatusUnauthorized, "unauthorized")

	issueResp := bindingRequest(t, e, http.MethodPost, "/api/v1/auth/external-binding-codes", oidcToken, nil)
	defer issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(issueResp.Body)
		t.Fatalf("issue status = %d: %s", issueResp.StatusCode, data)
	}
	var issued struct {
		Code      string `json:"code"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(issueResp.Body).Decode(&issued); err != nil {
		t.Fatal(err)
	}
	if issued.Code == "" || issued.ExpiresAt == 0 {
		t.Fatalf("issued binding code = %#v", issued)
	}

	redeemBody := map[string]any{
		"code": issued.Code, "adapter_id": "astrbot",
		"scope":   map[string]string{"type": "group", "id": "42"},
		"subject": map[string]string{"id": "7"},
	}
	redeemResp := bindingRequest(t, e, http.MethodPost, "/api/v1/integrations/bindings/redeem", created.Token, redeemBody)
	defer redeemResp.Body.Close()
	if redeemResp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(redeemResp.Body)
		t.Fatalf("redeem status = %d: %s", redeemResp.StatusCode, data)
	}
	var redeemed struct {
		Identity auth.Identity `json:"identity"`
		Binding  struct {
			PrincipalID string `json:"principal_id"`
		} `json:"binding"`
	}
	if err := json.NewDecoder(redeemResp.Body).Decode(&redeemed); err != nil {
		t.Fatal(err)
	}
	if redeemed.Identity.ID != principalID || redeemed.Binding.PrincipalID != principalID {
		t.Fatalf("redeemed binding = %#v", redeemed)
	}

	replayResp := bindingRequest(t, e, http.MethodPost, "/api/v1/integrations/bindings/redeem", created.Token, redeemBody)
	defer replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusOK || replayResp.Header.Get("Idempotency-Replayed") != "true" {
		data, _ := io.ReadAll(replayResp.Body)
		t.Fatalf("replay status = %d header = %q: %s", replayResp.StatusCode, replayResp.Header.Get("Idempotency-Replayed"), data)
	}

	resolveResp := bindingRequest(t, e, http.MethodPost, "/api/v1/integrations/actors/resolve", created.Token, map[string]any{
		"adapter_id": "astrbot",
		"scope":      map[string]string{"type": "group", "id": "42"},
		"subject":    map[string]string{"id": "7", "display_name": "Ignored external name"},
	})
	defer resolveResp.Body.Close()
	if resolveResp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resolveResp.Body)
		t.Fatalf("actor resolve status = %d: %s", resolveResp.StatusCode, data)
	}
	var resolved actorResolveResponse
	if err := json.NewDecoder(resolveResp.Body).Decode(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Identity.ID != principalID || resolved.Identity.Kind != "oidc" {
		t.Fatalf("resolved identity = %#v", resolved.Identity)
	}

	actorIssueResp := bindingRequest(t, e, http.MethodPost, "/api/v1/auth/external-binding-codes", resolved.ActorToken, nil)
	assertBindingError(t, actorIssueResp, http.StatusForbidden, "forbidden")
}

func bindingRequest(t *testing.T, e *oidcEnv, method, path, token string, body any) *http.Response {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func assertBindingError(t *testing.T, resp *http.Response, status int, code string) {
	t.Helper()
	defer resp.Body.Close()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status || body.Error.Code != code {
		t.Fatalf("binding error = status %d code %q, want %d %q", resp.StatusCode, body.Error.Code, status, code)
	}
}
