package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveIntegrationActorRejectsControlCharacterDisplayName(t *testing.T) {
	fixture := setupManagementQueries(t)
	body, err := json.Marshal(map[string]any{
		"adapter_id": "alpha",
		"scope": map[string]string{
			"type": "group",
			"id":   "1",
		},
		"subject": map[string]string{
			"id":           "control-character-user",
			"display_name": "line\nbreak",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/actors/resolve", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+managementIntegrationSecret)
	rec := httptest.NewRecorder()
	fixture.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "invalid_name" {
		t.Fatalf("error code = %q, want invalid_name", response.Error.Code)
	}
}
