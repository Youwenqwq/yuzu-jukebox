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

	refresh := authenticatedJSONRequest(t, handler, http.MethodPost,
		"/api/v1/accelerations/edgeone-main/inventory/refresh", adminToken, nil)
	if refresh.Code != http.StatusAccepted {
		t.Fatalf("inventory refresh = %d: %s", refresh.Code, refresh.Body.String())
	}
	var refreshResponse struct {
		Scan store.AccelerationInventoryScan `json:"scan"`
	}
	decodeRecorder(t, refresh, &refreshResponse)
	inventoryClaim := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/inventory/claim", createResponse.Credentials.PublisherToken,
		map[string]any{"owner": "publisher-1", "lease_seconds": 600})
	if inventoryClaim.Code != http.StatusOK {
		t.Fatalf("inventory claim = %d: %s", inventoryClaim.Code, inventoryClaim.Body.String())
	}
	inventoryComplete := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/inventory", createResponse.Credentials.PublisherToken,
		map[string]any{
			"owner": "publisher-1", "scan_id": refreshResponse.Scan.ID,
			"observed_at": 1_000_000, "objects": []any{}, "complete": true,
		})
	if inventoryComplete.Code != http.StatusNoContent {
		t.Fatalf("inventory complete = %d: %s", inventoryComplete.Code, inventoryComplete.Body.String())
	}
	inventoryStatus := authenticatedJSONRequest(t, handler, http.MethodGet,
		"/api/v1/accelerations/edgeone-main/inventory/status", adminToken, nil)
	if inventoryStatus.Code != http.StatusOK ||
		!strings.Contains(inventoryStatus.Body.String(), `"state":"completed"`) ||
		!strings.Contains(inventoryStatus.Body.String(), `"observed_object_count":0`) {
		t.Fatalf("inventory status = %d: %s", inventoryStatus.Code, inventoryStatus.Body.String())
	}

	if err := st.RequestDistribution(t.Context(), "edgeone-main", "local:queued", 1_000_010); err != nil {
		t.Fatal(err)
	}
	queuedCancel := authenticatedJSONRequest(t, handler, http.MethodDelete,
		"/api/v1/accelerations/edgeone-main/requests/local:queued", adminToken, nil)
	if queuedCancel.Code != http.StatusOK ||
		!strings.Contains(queuedCancel.Body.String(), `"state":"canceled"`) {
		t.Fatalf("queued cancel = %d: %s", queuedCancel.Code, queuedCancel.Body.String())
	}

	if err := st.RequestDistribution(t.Context(), "edgeone-main", "local:active", 1_000_020); err != nil {
		t.Fatal(err)
	}
	distributionClaim := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/leases", createResponse.Credentials.PublisherToken,
		map[string]any{"owner": "publisher-1", "lease_seconds": 600})
	if distributionClaim.Code != http.StatusCreated {
		t.Fatalf("distribution claim = %d: %s", distributionClaim.Code, distributionClaim.Body.String())
	}
	var claimResponse struct {
		Lease store.DistributionLease `json:"lease"`
	}
	decodeRecorder(t, distributionClaim, &claimResponse)
	activeCancel := authenticatedJSONRequest(t, handler, http.MethodDelete,
		"/api/v1/accelerations/edgeone-main/requests/local:active", adminToken, nil)
	if activeCancel.Code != http.StatusOK ||
		!strings.Contains(activeCancel.Body.String(), `"state":"cancel_requested"`) {
		t.Fatalf("active cancel = %d: %s", activeCancel.Code, activeCancel.Body.String())
	}
	leaseStatus := distributionRequest(t, handler, http.MethodGet,
		"/internal/v1/accelerations/leases/"+claimResponse.Lease.ID,
		createResponse.Credentials.PublisherToken, nil)
	if leaseStatus.Code != http.StatusOK ||
		!strings.Contains(leaseStatus.Body.String(), `"cancel_requested":true`) {
		t.Fatalf("lease status = %d: %s", leaseStatus.Code, leaseStatus.Body.String())
	}
	cancelComplete := distributionRequest(t, handler, http.MethodPost,
		"/internal/v1/accelerations/leases/"+claimResponse.Lease.ID+"/cancel",
		createResponse.Credentials.PublisherToken, map[string]any{"owner": "publisher-1"})
	if cancelComplete.Code != http.StatusOK {
		t.Fatalf("cancel complete = %d: %s", cancelComplete.Code, cancelComplete.Body.String())
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

func TestValidateAccelerationCachePolicy(t *testing.T) {
	const low = 65
	cases := []struct {
		name    string
		mode    string
		horizon int
		share   int
		wantErr bool
	}{
		{"默认配置", store.CacheModePrefetchAndHeat, 2, 20, false},
		{"份额等于低水位可用", store.CacheModePrefetchAndHeat, 2, low, false},
		// 钉住的对象 GC 动不了，份额越过低水位后回收目标永远够不到。
		{"份额越过低水位", store.CacheModePrefetchAndHeat, 2, low + 1, true},
		// 仅待播模式没有热集要保护，份额是死值，不做这条交叉校验。
		{"仅待播模式不校验份额", store.CacheModePrefetch, 2, 100, false},
		// 视界是仅待播模式唯一的需求来源，为 0 等于什么都不缓存。
		{"仅待播模式视界为零", store.CacheModePrefetch, 0, 20, true},
		{"混合模式视界为零合法", store.CacheModePrefetchAndHeat, 0, 20, false},
		{"视界超上限", store.CacheModePrefetchAndHeat, 21, 20, true},
		{"视界为负", store.CacheModePrefetchAndHeat, -1, 20, true},
		{"份额为零", store.CacheModePrefetchAndHeat, 2, 0, true},
		{"未知模式", "heat_only", 2, 20, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAccelerationCachePolicy(tc.mode, tc.horizon, tc.share, low)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate(%q, %d, %d, %d) = %v, wantErr %v",
					tc.mode, tc.horizon, tc.share, low, err, tc.wantErr)
			}
		})
	}
}
