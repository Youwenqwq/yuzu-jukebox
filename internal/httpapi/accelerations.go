package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/distribution"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

var (
	accelerationIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	accelerationHealthClient = &http.Client{Timeout: 10 * time.Second}
)

type accelerationView struct {
	ID                          string `json:"id"`
	Name                        string `json:"name"`
	Kind                        string `json:"kind"`
	Enabled                     bool   `json:"enabled"`
	CacheMode                   string `json:"cache_mode"`
	PrefetchHorizon             int    `json:"prefetch_horizon"`
	PrefetchSharePercent        int    `json:"prefetch_share_percent"`
	ControlBaseURL              string `json:"control_base_url"`
	BackendBaseURL              string `json:"backend_base_url"`
	LeaseTTLSeconds             int    `json:"lease_ttl_seconds"`
	UploadRateBytesPerSecond    int64  `json:"upload_rate_bytes_per_second"`
	MaxObjectBytes              int64  `json:"max_object_bytes"`
	StorageBudgetBytes          int64  `json:"storage_budget_bytes"`
	StorageHighWatermarkPercent int    `json:"storage_high_watermark_percent"`
	StorageLowWatermarkPercent  int    `json:"storage_low_watermark_percent"`
	InventoryIntervalSeconds    int    `json:"inventory_interval_seconds"`
	InventoryStaleAfterSeconds  int    `json:"inventory_stale_after_seconds"`
	PublisherConfigured         bool   `json:"publisher_credential_configured"`
	DeliveryConfigured          bool   `json:"delivery_credential_configured"`
	BackendConfigured           bool   `json:"backend_credential_configured"`
	PublisherPending            bool   `json:"publisher_credential_pending"`
	DeliveryPending             bool   `json:"delivery_credential_pending"`
	BackendPending              bool   `json:"backend_credential_pending"`
	ControlHealthy              *bool  `json:"control_healthy,omitempty"`
	BackendHealthy              *bool  `json:"backend_healthy,omitempty"`
	HealthError                 string `json:"health_error,omitempty"`
	LastHealthAt                *int64 `json:"last_health_at,omitempty"`
	CreatedAt                   int64  `json:"created_at"`
	UpdatedAt                   int64  `json:"updated_at"`
}

type createAccelerationRequest struct {
	ID                          string `json:"id"`
	Name                        string `json:"name"`
	Kind                        string `json:"kind"`
	ControlBaseURL              string `json:"control_base_url"`
	BackendBaseURL              string `json:"backend_base_url"`
	CacheMode                   string `json:"cache_mode"`
	PrefetchHorizon             *int   `json:"prefetch_horizon"`
	PrefetchSharePercent        *int   `json:"prefetch_share_percent"`
	LeaseTTLSeconds             int    `json:"lease_ttl_seconds"`
	UploadRateBytesPerSecond    *int64 `json:"upload_rate_bytes_per_second"`
	MaxObjectBytes              int64  `json:"max_object_bytes"`
	StorageBudgetBytes          int64  `json:"storage_budget_bytes"`
	StorageHighWatermarkPercent int    `json:"storage_high_watermark_percent"`
	StorageLowWatermarkPercent  int    `json:"storage_low_watermark_percent"`
	InventoryIntervalSeconds    int    `json:"inventory_interval_seconds"`
	InventoryStaleAfterSeconds  int    `json:"inventory_stale_after_seconds"`
}

type updateAccelerationRequest struct {
	Name                        *string `json:"name"`
	Enabled                     *bool   `json:"enabled"`
	CacheMode                   *string `json:"cache_mode"`
	PrefetchHorizon             *int    `json:"prefetch_horizon"`
	PrefetchSharePercent        *int    `json:"prefetch_share_percent"`
	ControlBaseURL              *string `json:"control_base_url"`
	BackendBaseURL              *string `json:"backend_base_url"`
	LeaseTTLSeconds             *int    `json:"lease_ttl_seconds"`
	UploadRateBytesPerSecond    *int64  `json:"upload_rate_bytes_per_second"`
	MaxObjectBytes              *int64  `json:"max_object_bytes"`
	StorageBudgetBytes          *int64  `json:"storage_budget_bytes"`
	StorageHighWatermarkPercent *int    `json:"storage_high_watermark_percent"`
	StorageLowWatermarkPercent  *int    `json:"storage_low_watermark_percent"`
	InventoryIntervalSeconds    *int    `json:"inventory_interval_seconds"`
	InventoryStaleAfterSeconds  *int    `json:"inventory_stale_after_seconds"`
}

func accelerationResponse(acceleration store.Acceleration) accelerationView {
	return accelerationView{
		ID: acceleration.ID, Name: acceleration.Name, Kind: acceleration.Kind,
		Enabled: acceleration.Enabled, CacheMode: acceleration.CacheMode,
		PrefetchHorizon:      acceleration.PrefetchHorizon,
		PrefetchSharePercent: acceleration.PrefetchSharePercent,
		ControlBaseURL:       acceleration.ControlBaseURL, BackendBaseURL: acceleration.BackendBaseURL,
		LeaseTTLSeconds:             acceleration.LeaseTTLSeconds,
		UploadRateBytesPerSecond:    acceleration.UploadRateBytesPerSecond,
		MaxObjectBytes:              acceleration.MaxObjectBytes,
		StorageBudgetBytes:          acceleration.StorageBudgetBytes,
		StorageHighWatermarkPercent: acceleration.StorageHighWatermarkPercent,
		StorageLowWatermarkPercent:  acceleration.StorageLowWatermarkPercent,
		InventoryIntervalSeconds:    acceleration.InventoryIntervalSeconds,
		InventoryStaleAfterSeconds:  acceleration.InventoryStaleAfterSeconds,
		PublisherConfigured:         len(acceleration.PublisherTokenHash) != 0,
		DeliveryConfigured:          len(acceleration.DeliveryTokenHash) != 0,
		BackendConfigured:           acceleration.BackendToken != "",
		PublisherPending:            len(acceleration.PublisherPendingTokenHash) != 0,
		DeliveryPending:             len(acceleration.DeliveryPendingTokenHash) != 0,
		BackendPending:              acceleration.BackendPendingToken != "",
		ControlHealthy:              acceleration.ControlHealthy, BackendHealthy: acceleration.BackendHealthy,
		HealthError: acceleration.HealthError, LastHealthAt: acceleration.LastHealthAt,
		CreatedAt: acceleration.CreatedAt, UpdatedAt: acceleration.UpdatedAt,
	}
}

func (s *Server) listAccelerations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleMediaAdmin); !ok {
		return
	}
	rows, err := s.st.ListAccelerations(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to list accelerations")
		return
	}
	views := make([]accelerationView, len(rows))
	for index, row := range rows {
		views[index] = accelerationResponse(row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accelerations": views})
}

func (s *Server) getAcceleration(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleMediaAdmin); !ok {
		return
	}
	acceleration, err := s.st.GetAcceleration(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "acceleration not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load acceleration")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"acceleration": accelerationResponse(acceleration)})
}

func (s *Server) createAcceleration(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	var body createAccelerationRequest
	if err := decodeIntegrationJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	body.Name = strings.TrimSpace(body.Name)
	body.Kind = strings.TrimSpace(body.Kind)
	if body.Kind == "" {
		body.Kind = "edgeone"
	}
	if !accelerationIDPattern.MatchString(body.ID) || body.Name == "" || len(body.Name) > 100 || body.Kind != "edgeone" {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid acceleration id, name, or kind")
		return
	}
	controlURL, backendURL, err := validateAccelerationURLs(body.ControlBaseURL, body.BackendBaseURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	cacheMode := strings.TrimSpace(body.CacheMode)
	if cacheMode == "" {
		cacheMode = store.CacheModePrefetchAndHeat
	}
	prefetchHorizon := store.DefaultPrefetchHorizon
	if body.PrefetchHorizon != nil {
		prefetchHorizon = *body.PrefetchHorizon
	}
	prefetchShare := store.DefaultPrefetchSharePercent
	if body.PrefetchSharePercent != nil {
		prefetchShare = *body.PrefetchSharePercent
	}
	if body.LeaseTTLSeconds == 0 {
		body.LeaseTTLSeconds = 600
	}
	uploadRate := int64(187_500)
	if body.UploadRateBytesPerSecond != nil {
		uploadRate = *body.UploadRateBytesPerSecond
	}
	if body.MaxObjectBytes == 0 {
		body.MaxObjectBytes = 23 << 20
	}
	if body.StorageBudgetBytes == 0 {
		body.StorageBudgetBytes = 850 << 20
	}
	if body.StorageHighWatermarkPercent == 0 {
		body.StorageHighWatermarkPercent = 95
	}
	if body.StorageLowWatermarkPercent == 0 {
		body.StorageLowWatermarkPercent = 85
	}
	if body.InventoryIntervalSeconds == 0 {
		body.InventoryIntervalSeconds = 900
	}
	if body.InventoryStaleAfterSeconds == 0 {
		body.InventoryStaleAfterSeconds = 1800
	}
	if body.LeaseTTLSeconds <= 0 || uploadRate < 0 || body.MaxObjectBytes <= 0 ||
		body.StorageBudgetBytes <= 0 || body.StorageLowWatermarkPercent <= 0 ||
		body.StorageHighWatermarkPercent > 100 ||
		body.StorageLowWatermarkPercent >= body.StorageHighWatermarkPercent ||
		body.InventoryIntervalSeconds < 60 ||
		body.InventoryStaleAfterSeconds < body.InventoryIntervalSeconds {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid acceleration limits")
		return
	}
	if err := validateAccelerationCachePolicy(cacheMode, prefetchHorizon, prefetchShare,
		body.StorageLowWatermarkPercent); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	publisherToken, publisherHash, err := distribution.NewCredential("publisher")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to create publisher credential")
		return
	}
	deliveryToken, deliveryHash, err := distribution.NewCredential("delivery")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to create delivery credential")
		return
	}
	backendToken, _, err := distribution.NewCredential("backend")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to create backend credential")
		return
	}
	acceleration, err := s.st.CreateAcceleration(r.Context(), store.Acceleration{
		ID: body.ID, Name: body.Name, Kind: body.Kind,
		CacheMode: cacheMode, PrefetchHorizon: prefetchHorizon,
		PrefetchSharePercent: prefetchShare, ControlBaseURL: controlURL,
		BackendBaseURL: backendURL, LeaseTTLSeconds: body.LeaseTTLSeconds,
		UploadRateBytesPerSecond: uploadRate, MaxObjectBytes: body.MaxObjectBytes,
		StorageBudgetBytes:          body.StorageBudgetBytes,
		StorageHighWatermarkPercent: body.StorageHighWatermarkPercent,
		StorageLowWatermarkPercent:  body.StorageLowWatermarkPercent,
		InventoryIntervalSeconds:    body.InventoryIntervalSeconds,
		InventoryStaleAfterSeconds:  body.InventoryStaleAfterSeconds,
	}, publisherHash, deliveryHash, backendToken)
	if err != nil {
		writeErr(w, http.StatusConflict, "conflict", "acceleration already exists")
		return
	}
	s.audit(r.Context(), actor.ID, "acceleration.create", acceleration.ID, map[string]any{
		"name": acceleration.Name, "kind": acceleration.Kind,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"acceleration": accelerationResponse(acceleration),
		"credentials": map[string]string{
			"publisher_token": publisherToken,
			"delivery_token":  deliveryToken,
			"backend_token":   backendToken,
		},
	})
}

func (s *Server) updateAcceleration(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	current, err := s.st.GetAcceleration(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "acceleration not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load acceleration")
		return
	}
	var body updateAccelerationRequest
	if err := decodeIntegrationJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	update := store.AccelerationUpdate{
		Name: current.Name, Enabled: current.Enabled,
		CacheMode:            current.CacheMode,
		PrefetchHorizon:      current.PrefetchHorizon,
		PrefetchSharePercent: current.PrefetchSharePercent,
		ControlBaseURL:       current.ControlBaseURL, BackendBaseURL: current.BackendBaseURL,
		LeaseTTLSeconds:             current.LeaseTTLSeconds,
		UploadRateBytesPerSecond:    current.UploadRateBytesPerSecond,
		MaxObjectBytes:              current.MaxObjectBytes,
		StorageBudgetBytes:          current.StorageBudgetBytes,
		StorageHighWatermarkPercent: current.StorageHighWatermarkPercent,
		StorageLowWatermarkPercent:  current.StorageLowWatermarkPercent,
		InventoryIntervalSeconds:    current.InventoryIntervalSeconds,
		InventoryStaleAfterSeconds:  current.InventoryStaleAfterSeconds,
	}
	if body.Name != nil {
		update.Name = strings.TrimSpace(*body.Name)
	}
	if body.CacheMode != nil {
		update.CacheMode = strings.TrimSpace(*body.CacheMode)
	}
	if body.PrefetchHorizon != nil {
		update.PrefetchHorizon = *body.PrefetchHorizon
	}
	if body.PrefetchSharePercent != nil {
		update.PrefetchSharePercent = *body.PrefetchSharePercent
	}
	if body.ControlBaseURL != nil {
		update.ControlBaseURL = strings.TrimSpace(*body.ControlBaseURL)
	}
	if body.BackendBaseURL != nil {
		update.BackendBaseURL = strings.TrimSpace(*body.BackendBaseURL)
	}
	if body.LeaseTTLSeconds != nil {
		update.LeaseTTLSeconds = *body.LeaseTTLSeconds
	}
	if body.UploadRateBytesPerSecond != nil {
		update.UploadRateBytesPerSecond = *body.UploadRateBytesPerSecond
	}
	if body.MaxObjectBytes != nil {
		update.MaxObjectBytes = *body.MaxObjectBytes
	}
	if body.StorageBudgetBytes != nil {
		update.StorageBudgetBytes = *body.StorageBudgetBytes
	}
	if body.StorageHighWatermarkPercent != nil {
		update.StorageHighWatermarkPercent = *body.StorageHighWatermarkPercent
	}
	if body.StorageLowWatermarkPercent != nil {
		update.StorageLowWatermarkPercent = *body.StorageLowWatermarkPercent
	}
	if body.InventoryIntervalSeconds != nil {
		update.InventoryIntervalSeconds = *body.InventoryIntervalSeconds
	}
	if body.InventoryStaleAfterSeconds != nil {
		update.InventoryStaleAfterSeconds = *body.InventoryStaleAfterSeconds
	}
	if body.Enabled != nil {
		update.Enabled = *body.Enabled
	}
	if update.Name == "" || len(update.Name) > 100 || update.LeaseTTLSeconds <= 0 ||
		update.UploadRateBytesPerSecond < 0 || update.MaxObjectBytes <= 0 ||
		update.StorageBudgetBytes <= 0 || update.StorageLowWatermarkPercent <= 0 ||
		update.StorageHighWatermarkPercent > 100 ||
		update.StorageLowWatermarkPercent >= update.StorageHighWatermarkPercent ||
		update.InventoryIntervalSeconds < 60 ||
		update.InventoryStaleAfterSeconds < update.InventoryIntervalSeconds {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid acceleration configuration")
		return
	}
	if err := validateAccelerationCachePolicy(update.CacheMode, update.PrefetchHorizon,
		update.PrefetchSharePercent, update.StorageLowWatermarkPercent); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if update.ControlBaseURL, update.BackendBaseURL, err = validateAccelerationURLs(update.ControlBaseURL, update.BackendBaseURL); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var controlOK, backendOK bool
	var healthDetail string
	if !current.Enabled && update.Enabled {
		current.Name = update.Name
		current.CacheMode = update.CacheMode
		current.PrefetchHorizon = update.PrefetchHorizon
		current.PrefetchSharePercent = update.PrefetchSharePercent
		current.ControlBaseURL = update.ControlBaseURL
		current.BackendBaseURL = update.BackendBaseURL
		current.LeaseTTLSeconds = update.LeaseTTLSeconds
		current.UploadRateBytesPerSecond = update.UploadRateBytesPerSecond
		current.MaxObjectBytes = update.MaxObjectBytes
		current.StorageBudgetBytes = update.StorageBudgetBytes
		current.StorageHighWatermarkPercent = update.StorageHighWatermarkPercent
		current.StorageLowWatermarkPercent = update.StorageLowWatermarkPercent
		current.InventoryIntervalSeconds = update.InventoryIntervalSeconds
		current.InventoryStaleAfterSeconds = update.InventoryStaleAfterSeconds
		controlOK, backendOK, healthDetail = distribution.CheckHealth(
			r.Context(), accelerationHealthClient, current, current.BackendToken,
		)
		update.HealthChecked = true
		update.ControlHealthy = controlOK
		update.BackendHealthy = backendOK
		update.HealthError = healthDetail
		update.HealthCheckedAt = time.Now().UnixMilli()
		current.ControlHealthy = &controlOK
		current.BackendHealthy = &backendOK
		current.HealthError = healthDetail
		ready, problems, readyErr := s.st.AccelerationReadyFor(r.Context(), current, time.Now().Add(-45*time.Second).UnixMilli())
		if readyErr != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to check acceleration readiness")
			return
		}
		if !ready {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": map[string]any{"code": "acceleration_not_ready", "message": "acceleration is not ready", "problems": problems},
			})
			return
		}
	}
	updated, err := s.st.UpdateAcceleration(r.Context(), current.ID, update)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to update acceleration")
		return
	}
	action := "acceleration.update"
	if current.Enabled != updated.Enabled {
		if updated.Enabled {
			action = "acceleration.enable"
		} else {
			action = "acceleration.disable"
		}
	}
	s.audit(r.Context(), actor.ID, action, updated.ID, map[string]any{"enabled": updated.Enabled})
	writeJSON(w, http.StatusOK, map[string]any{"acceleration": accelerationResponse(updated)})
}

func (s *Server) deleteAcceleration(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	id := r.PathValue("id")
	err := s.st.DeleteAcceleration(r.Context(), id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", "acceleration not found")
		return
	case errors.Is(err, store.ErrAccelerationNotEmpty):
		writeErr(w, http.StatusConflict, "acceleration_not_empty", "acceleration still owns media or active work")
		return
	case err != nil:
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	s.audit(r.Context(), actor.ID, "acceleration.delete", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) prepareAccelerationCredential(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	purpose := r.PathValue("purpose")
	token, hash, err := distribution.NewCredential(purpose)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid credential purpose")
		return
	}
	acceleration, err := s.st.PrepareAccelerationCredential(r.Context(), r.PathValue("id"), purpose, hash, token)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "acceleration not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to prepare acceleration credential")
		return
	}
	s.audit(r.Context(), actor.ID, "acceleration.credential.prepare", acceleration.ID, map[string]any{"purpose": purpose})
	writeJSON(w, http.StatusOK, map[string]any{"acceleration": accelerationResponse(acceleration), "token": token})
}

func (s *Server) activateAccelerationCredential(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	id, purpose := r.PathValue("id"), r.PathValue("purpose")
	switch purpose {
	case "publisher", "delivery", "backend":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid credential purpose")
		return
	}
	current, err := s.st.GetAcceleration(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "acceleration not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load acceleration")
		return
	}
	if purpose == "backend" {
		if current.BackendPendingToken == "" {
			writeErr(w, http.StatusConflict, "credential_not_pending", "backend credential is not pending")
			return
		}
		_, backendOK, detail := distribution.CheckHealth(r.Context(), accelerationHealthClient, current, current.BackendPendingToken)
		if !backendOK {
			writeErr(w, http.StatusConflict, "credential_not_ready", detail)
			return
		}
	}
	acceleration, err := s.st.ActivateAccelerationCredential(r.Context(), id, purpose)
	if errors.Is(err, store.ErrAccelerationNoCredential) {
		writeErr(w, http.StatusConflict, "credential_not_pending", "acceleration credential is not pending")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to activate acceleration credential")
		return
	}
	s.audit(r.Context(), actor.ID, "acceleration.credential.activate", id, map[string]any{"purpose": purpose})
	writeJSON(w, http.StatusOK, map[string]any{"acceleration": accelerationResponse(acceleration)})
}

func (s *Server) accelerationStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleMediaAdmin); !ok {
		return
	}
	id := r.PathValue("id")
	acceleration, err := s.st.GetAcceleration(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "acceleration not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load acceleration")
		return
	}
	metrics, summary, err := s.distribution.Metrics(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load acceleration metrics")
		return
	}
	rolling, err := s.distribution.Metrics24Hours(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load rolling metrics")
		return
	}
	publishers, err := s.st.ListAccelerationPublishers(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load publishers")
		return
	}
	attempts, err := s.st.ListDistributionAttempts(r.Context(), id, "active", 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load active uploads")
		return
	}
	storageStatus, err := s.st.AccelerationStorageStatus(r.Context(), id, time.Now().UnixMilli())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load acceleration storage status")
		return
	}
	var inventoryScan any
	latestScan, scanErr := s.st.LatestAccelerationInventoryScan(r.Context(), id)
	if scanErr == nil {
		inventoryScan = latestScan
	} else if !errors.Is(scanErr, sql.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load inventory scan")
		return
	}
	now := time.Now().UnixMilli()
	publisherViews := make([]map[string]any, 0, len(publishers))
	for _, publisher := range publishers {
		var capabilities []string
		_ = json.Unmarshal([]byte(publisher.Capabilities), &capabilities)
		publisherViews = append(publisherViews, map[string]any{
			"owner": publisher.Owner, "version": publisher.Version,
			"state": publisher.State, "online": publisher.LastSeenAt >= now-45_000,
			"lease_id": publisher.LeaseID, "track_ref": publisher.TrackRef,
			"capabilities": capabilities, "backend_healthy": publisher.BackendHealthy,
			"last_error": publisher.LastError, "last_seen_at": publisher.LastSeenAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"acceleration": accelerationResponse(acceleration), "summary": summary,
		"storage": storageStatus, "inventory_scan": inventoryScan,
		"publishers": publisherViews, "active": attempts,
		"counters": metrics, "last_24_hours": rolling,
	})
}

func (s *Server) accelerationRequests(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleMediaAdmin); !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := s.st.GetAcceleration(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "acceleration not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load acceleration")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.st.ListDistributionRequests(r.Context(), id,
		r.URL.Query().Get("state"), time.Now().UnixMilli(), limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": rows})
}

func (s *Server) accelerationRequest(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleMediaAdmin); !ok {
		return
	}
	row, err := s.st.GetDistributionRequest(r.Context(), r.PathValue("id"),
		r.PathValue("track_ref"), time.Now().UnixMilli())
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "distribution request not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load distribution request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": row})
}
func (s *Server) cancelAccelerationRequest(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	id := r.PathValue("id")
	trackRef := r.PathValue("track_ref")
	now := time.Now().UnixMilli()
	err := s.st.RequestDistributionCancellation(r.Context(), id, trackRef, now)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not_found", "distribution request not found")
		return
	case errors.Is(err, store.ErrDistributionRequestReady):
		writeErr(w, http.StatusConflict, "request_ready", "distribution request is already ready")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", "failed to cancel distribution request")
		return
	}
	s.audit(r.Context(), actor.ID, "acceleration.request.cancel", id,
		map[string]any{"track_ref": trackRef})
	row, err := s.st.GetDistributionRequest(r.Context(), id, trackRef, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load canceled request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": row})
}

func (s *Server) refreshAccelerationInventory(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleMediaAdmin)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := s.st.GetAcceleration(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "acceleration not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load acceleration")
		return
	}
	scan, err := s.st.RequestAccelerationInventoryScan(r.Context(), id, time.Now().UnixMilli())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to request inventory refresh")
		return
	}
	s.audit(r.Context(), actor.ID, "acceleration.inventory.refresh", id,
		map[string]any{"scan_id": scan.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"scan": scan})
}

func (s *Server) accelerationInventoryStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleMediaAdmin); !ok {
		return
	}
	id := r.PathValue("id")
	storageStatus, err := s.st.AccelerationStorageStatus(r.Context(), id, time.Now().UnixMilli())
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "acceleration not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load inventory status")
		return
	}
	var scan any
	latest, err := s.st.LatestAccelerationInventoryScan(r.Context(), id)
	if err == nil {
		scan = latest
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load inventory scan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"storage": storageStatus, "scan": scan})
}

// validateAccelerationCachePolicy 校验缓存模式与两个预取参数。
//
// prefetch_share_percent 必须不大于低水位：GC 的回收目标是低水位，而被钉住的部分
// 它动不了。份额上限一旦越过低水位，GC 永远够不到目标，资源会陷入永久的存储压力。
func validateAccelerationCachePolicy(mode string, horizon, sharePercent, lowWatermark int) error {
	if mode != store.CacheModePrefetch && mode != store.CacheModePrefetchAndHeat {
		return fmt.Errorf("cache_mode must be %q or %q",
			store.CacheModePrefetch, store.CacheModePrefetchAndHeat)
	}
	if horizon < 0 || horizon > 20 {
		return errors.New("prefetch_horizon must be between 0 and 20")
	}
	if mode == store.CacheModePrefetch && horizon == 0 {
		return errors.New("prefetch_horizon must be at least 1 in prefetch mode; " +
			"the queue horizon is the only demand source, so 0 would cache nothing")
	}
	if sharePercent < 1 || sharePercent > 100 {
		return errors.New("prefetch_share_percent must be between 1 and 100")
	}
	if mode == store.CacheModePrefetchAndHeat && sharePercent > lowWatermark {
		return errors.New("prefetch_share_percent must not exceed storage_low_watermark_percent; " +
			"pinned objects are not reclaimable, so a larger share makes the low watermark unreachable")
	}
	return nil
}

func validateAccelerationURLs(controlRaw, backendRaw string) (string, string, error) {
	control, err := validateAccelerationURL(controlRaw)
	if err != nil {
		return "", "", fmt.Errorf("invalid control_base_url: %w", err)
	}
	backend, err := validateAccelerationURL(backendRaw)
	if err != nil {
		return "", "", fmt.Errorf("invalid backend_base_url: %w", err)
	}
	return control, backend, nil
}

func validateAccelerationURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("absolute URL without credentials, query, or fragment required")
	}
	host := parsed.Hostname()
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (host == "localhost" || net.ParseIP(host) != nil)) {
		return "", errors.New("https is required except for localhost or IP development endpoints")
	}
	return raw, nil
}
