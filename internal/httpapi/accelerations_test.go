package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/distribution"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

func TestAccelerationManagementLifecycle(t *testing.T) {
	var backendToken string
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/yuzu-edge/health":
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		case "/yuzu-blob/health":
			if r.Header.Get("Authorization") != "Bearer "+backendToken {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "wrong backend token")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer health.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "accelerations.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	authm := auth.NewManager("", st)
	adminToken := authm.IssueSession(auth.Identity{
		ID: "media-admin", Name: "Media Admin", Roles: []string{auth.RoleMediaAdmin},
	})
	service := distribution.New(st)
	server := &Server{st: st, authm: authm}
	server.ConfigureDistribution(service, distribution.NewRegistry(st))
	handler := server.Handler()

	created := authenticatedJSONRequest(t, handler, http.MethodPost, "/api/v1/accelerations", adminToken, map[string]any{
		"id": "edgeone-main", "name": "Main EdgeOne", "kind": "edgeone",
		"control_base_url": health.URL + "/yuzu-edge",
		"backend_base_url": health.URL + "/yuzu-blob",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	var createResponse struct {
		Acceleration accelerationView `json:"acceleration"`
		Credentials  struct {
			PublisherToken string `json:"publisher_token"`
			DeliveryToken  string `json:"delivery_token"`
			BackendToken   string `json:"backend_token"`
		} `json:"credentials"`
	}
	decodeRecorder(t, created, &createResponse)
	backendToken = createResponse.Credentials.BackendToken
	if createResponse.Acceleration.Enabled || createResponse.Credentials.PublisherToken == "" ||
		createResponse.Credentials.DeliveryToken == "" || backendToken == "" {
		t.Fatalf("created acceleration = %#v", createResponse)
	}

	listed := authenticatedJSONRequest(t, handler, http.MethodGet, "/api/v1/accelerations", adminToken, nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", listed.Code, listed.Body.String())
	}
	for _, secret := range []string{createResponse.Credentials.PublisherToken, createResponse.Credentials.DeliveryToken, backendToken} {
		if strings.Contains(listed.Body.String(), secret) {
			t.Fatalf("list leaked credential %q", secret)
		}
	}

	config := distributionRequest(t, handler, http.MethodGet,
		"/internal/v1/accelerations/publisher/config", createResponse.Credentials.PublisherToken, nil)
	if config.Code != http.StatusOK || !strings.Contains(config.Body.String(), backendToken) {
		t.Fatalf("publisher config = %d: %s", config.Code, config.Body.String())
	}
	notReady := authenticatedJSONRequest(t, handler, http.MethodPatch,
		"/api/v1/accelerations/edgeone-main", adminToken, map[string]any{"enabled": true})
	if notReady.Code != http.StatusConflict ||
		!strings.Contains(notReady.Body.String(), "publisher_offline") {
		t.Fatalf("enable without publisher = %d: %s", notReady.Code, notReady.Body.String())
	}

	heartbeat := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/publishers/heartbeat", createResponse.Credentials.PublisherToken,
		map[string]any{
			"owner": "publisher-1", "version": "test", "state": "idle",
			"lease_id": "", "track_ref": "",
			"capabilities":    []string{"object.publish", "storage.inventory", "object.delete"},
			"backend_healthy": true, "last_error": "",
		})
	if heartbeat.Code != http.StatusNoContent {
		t.Fatalf("heartbeat = %d: %s", heartbeat.Code, heartbeat.Body.String())
	}

	enabled := authenticatedJSONRequest(t, handler, http.MethodPatch,
		"/api/v1/accelerations/edgeone-main", adminToken, map[string]any{"enabled": true})
	if enabled.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", enabled.Code, enabled.Body.String())
	}
	var enabledResponse struct {
		Acceleration accelerationView `json:"acceleration"`
	}
	decodeRecorder(t, enabled, &enabledResponse)
	if !enabledResponse.Acceleration.Enabled {
		t.Fatal("acceleration was not enabled")
	}

	status := authenticatedJSONRequest(t, handler, http.MethodGet,
		"/api/v1/accelerations/edgeone-main/status", adminToken, nil)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"online":true`) ||
		!strings.Contains(status.Body.String(), `"last_24_hours"`) {
		t.Fatalf("status = %d: %s", status.Code, status.Body.String())
	}

	prepared := authenticatedJSONRequest(t, handler, http.MethodPost,
		"/api/v1/accelerations/edgeone-main/credentials/publisher/prepare", adminToken, nil)
	if prepared.Code != http.StatusOK {
		t.Fatalf("prepare publisher token = %d: %s", prepared.Code, prepared.Body.String())
	}
	var preparedResponse struct {
		Token string `json:"token"`
	}
	decodeRecorder(t, prepared, &preparedResponse)
	if preparedResponse.Token == "" {
		t.Fatal("prepared token is empty")
	}
	pendingConfig := distributionRequest(t, handler, http.MethodGet,
		"/internal/v1/accelerations/publisher/config", preparedResponse.Token, nil)
	if pendingConfig.Code != http.StatusOK {
		t.Fatalf("pending token = %d: %s", pendingConfig.Code, pendingConfig.Body.String())
	}
	activated := authenticatedJSONRequest(t, handler, http.MethodPost,
		"/api/v1/accelerations/edgeone-main/credentials/publisher/activate", adminToken, nil)
	if activated.Code != http.StatusOK {
		t.Fatalf("activate publisher token = %d: %s", activated.Code, activated.Body.String())
	}
	oldConfig := distributionRequest(t, handler, http.MethodGet,
		"/internal/v1/accelerations/publisher/config", createResponse.Credentials.PublisherToken, nil)
	if oldConfig.Code != http.StatusUnauthorized {
		t.Fatalf("old token after activation = %d", oldConfig.Code)
	}
}

func authenticatedJSONRequest(
	t *testing.T,
	handler http.Handler,
	method, path, token string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, path, nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request = httptest.NewRequest(method, path, strings.NewReader(string(data)))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
