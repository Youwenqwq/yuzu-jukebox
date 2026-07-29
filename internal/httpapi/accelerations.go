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
	ID                       string `json:"id"`
	Name                     string `json:"name"`
	Kind                     string `json:"kind"`
	Enabled                  bool   `json:"enabled"`
	PublishOnCacheReady      bool   `json:"publish_on_cache_ready"`
	ControlBaseURL           string `json:"control_base_url"`
	SignerBaseURL            string `json:"signer_base_url"`
	LeaseTTLSeconds          int    `json:"lease_ttl_seconds"`
	UploadRateBytesPerSecond int64  `json:"upload_rate_bytes_per_second"`
	MaxObjectBytes           int64  `json:"max_object_bytes"`
	PublisherConfigured      bool   `json:"publisher_credential_configured"`
	EdgeConfigured           bool   `json:"edge_credential_configured"`
	SignerConfigured         bool   `json:"signer_credential_configured"`
	PublisherPending         bool   `json:"publisher_credential_pending"`
	EdgePending              bool   `json:"edge_credential_pending"`
	SignerPending            bool   `json:"signer_credential_pending"`
	ControlHealthy           *bool  `json:"control_healthy,omitempty"`
	SignerHealthy            *bool  `json:"signer_healthy,omitempty"`
	HealthError              string `json:"health_error,omitempty"`
	LastHealthAt             *int64 `json:"last_health_at,omitempty"`
	CreatedAt                int64  `json:"created_at"`
	UpdatedAt                int64  `json:"updated_at"`
}

type createAccelerationRequest struct {
	ID                       string `json:"id"`
	Name                     string `json:"name"`
	Kind                     string `json:"kind"`
	ControlBaseURL           string `json:"control_base_url"`
	SignerBaseURL            string `json:"signer_base_url"`
	PublishOnCacheReady      *bool  `json:"publish_on_cache_ready"`
	LeaseTTLSeconds          int    `json:"lease_ttl_seconds"`
	UploadRateBytesPerSecond *int64 `json:"upload_rate_bytes_per_second"`
	MaxObjectBytes           int64  `json:"max_object_bytes"`
}

type updateAccelerationRequest struct {
	Name                     *string `json:"name"`
	Enabled                  *bool   `json:"enabled"`
	PublishOnCacheReady      *bool   `json:"publish_on_cache_ready"`
	ControlBaseURL           *string `json:"control_base_url"`
	SignerBaseURL            *string `json:"signer_base_url"`
	LeaseTTLSeconds          *int    `json:"lease_ttl_seconds"`
	UploadRateBytesPerSecond *int64  `json:"upload_rate_bytes_per_second"`
	MaxObjectBytes           *int64  `json:"max_object_bytes"`
}

func accelerationResponse(acceleration store.Acceleration) accelerationView {
	return accelerationView{
		ID: acceleration.ID, Name: acceleration.Name, Kind: acceleration.Kind,
		Enabled: acceleration.Enabled, PublishOnCacheReady: acceleration.PublishOnCacheReady,
		ControlBaseURL: acceleration.ControlBaseURL, SignerBaseURL: acceleration.SignerBaseURL,
		LeaseTTLSeconds:          acceleration.LeaseTTLSeconds,
		UploadRateBytesPerSecond: acceleration.UploadRateBytesPerSecond,
		MaxObjectBytes:           acceleration.MaxObjectBytes,
		PublisherConfigured:      len(acceleration.PublisherTokenHash) != 0,
		EdgeConfigured:           len(acceleration.EdgeTokenHash) != 0,
		SignerConfigured:         acceleration.SignerToken != "",
		PublisherPending:         len(acceleration.PublisherPendingTokenHash) != 0,
		EdgePending:              len(acceleration.EdgePendingTokenHash) != 0,
		SignerPending:            acceleration.SignerPendingToken != "",
		ControlHealthy:           acceleration.ControlHealthy, SignerHealthy: acceleration.SignerHealthy,
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
	controlURL, signerURL, err := validateAccelerationURLs(body.ControlBaseURL, body.SignerBaseURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	publishOnReady := true
	if body.PublishOnCacheReady != nil {
		publishOnReady = *body.PublishOnCacheReady
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
	if body.LeaseTTLSeconds <= 0 || uploadRate < 0 || body.MaxObjectBytes <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid acceleration limits")
		return
	}
	publisherToken, publisherHash, err := distribution.NewCredential("publisher")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to create publisher credential")
		return
	}
	edgeToken, edgeHash, err := distribution.NewCredential("edge")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to create edge credential")
		return
	}
	signerToken, _, err := distribution.NewCredential("signer")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to create signer credential")
		return
	}
	acceleration, err := s.st.CreateAcceleration(r.Context(), store.Acceleration{
		ID: body.ID, Name: body.Name, Kind: body.Kind,
		PublishOnCacheReady: publishOnReady, ControlBaseURL: controlURL,
		SignerBaseURL: signerURL, LeaseTTLSeconds: body.LeaseTTLSeconds,
		UploadRateBytesPerSecond: uploadRate, MaxObjectBytes: body.MaxObjectBytes,
	}, publisherHash, edgeHash, signerToken)
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
			"edge_token":      edgeToken,
			"signer_token":    signerToken,
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
		PublishOnCacheReady: current.PublishOnCacheReady,
		ControlBaseURL:      current.ControlBaseURL, SignerBaseURL: current.SignerBaseURL,
		LeaseTTLSeconds:          current.LeaseTTLSeconds,
		UploadRateBytesPerSecond: current.UploadRateBytesPerSecond,
		MaxObjectBytes:           current.MaxObjectBytes,
	}
	if body.Name != nil {
		update.Name = strings.TrimSpace(*body.Name)
	}
	if body.PublishOnCacheReady != nil {
		update.PublishOnCacheReady = *body.PublishOnCacheReady
	}
	if body.ControlBaseURL != nil {
		update.ControlBaseURL = strings.TrimSpace(*body.ControlBaseURL)
	}
	if body.SignerBaseURL != nil {
		update.SignerBaseURL = strings.TrimSpace(*body.SignerBaseURL)
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
	if body.Enabled != nil {
		update.Enabled = *body.Enabled
	}
	if update.Name == "" || len(update.Name) > 100 || update.LeaseTTLSeconds <= 0 ||
		update.UploadRateBytesPerSecond < 0 || update.MaxObjectBytes <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid acceleration configuration")
		return
	}
	if update.ControlBaseURL, update.SignerBaseURL, err = validateAccelerationURLs(update.ControlBaseURL, update.SignerBaseURL); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var healthChecked bool
	var controlOK, signerOK bool
	var healthDetail string
	var healthCheckedAt int64
	if !current.Enabled && update.Enabled {
		current.Name = update.Name
		current.PublishOnCacheReady = update.PublishOnCacheReady
		current.ControlBaseURL = update.ControlBaseURL
		current.SignerBaseURL = update.SignerBaseURL
		current.LeaseTTLSeconds = update.LeaseTTLSeconds
		current.UploadRateBytesPerSecond = update.UploadRateBytesPerSecond
		current.MaxObjectBytes = update.MaxObjectBytes
		controlOK, signerOK, healthDetail = distribution.CheckHealth(
			r.Context(), accelerationHealthClient, current, current.SignerToken,
		)
		healthChecked = true
		healthCheckedAt = time.Now().UnixMilli()
		current.ControlHealthy = &controlOK
		current.SignerHealthy = &signerOK
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
	if healthChecked {
		if err := s.st.UpdateAccelerationHealth(
			r.Context(), updated.ID, controlOK, signerOK, healthDetail, healthCheckedAt,
		); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to persist acceleration health")
			return
		}
		updated.ControlHealthy = &controlOK
		updated.SignerHealthy = &signerOK
		updated.HealthError = healthDetail
		updated.LastHealthAt = &healthCheckedAt
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
		writeErr(w, http.StatusConflict, "acceleration_not_empty", "acceleration still owns leases or candidates")
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
	case "publisher", "edge", "signer":
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
	if purpose == "signer" {
		if current.SignerPendingToken == "" {
			writeErr(w, http.StatusConflict, "credential_not_pending", "signer credential is not pending")
			return
		}
		_, signerOK, detail := distribution.CheckHealth(r.Context(), accelerationHealthClient, current, current.SignerPendingToken)
		if !signerOK {
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
		"publishers": publisherViews, "active": attempts,
		"counters": metrics, "last_24_hours": rolling,
	})
}

func (s *Server) accelerationRequests(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleMediaAdmin); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.st.ListDistributionRequests(r.Context(), r.PathValue("id"),
		r.URL.Query().Get("state"), time.Now().UnixMilli(), limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": rows})
}

func validateAccelerationURLs(controlRaw, signerRaw string) (string, string, error) {
	control, err := validateAccelerationURL(controlRaw)
	if err != nil {
		return "", "", fmt.Errorf("invalid control_base_url: %w", err)
	}
	signer, err := validateAccelerationURL(signerRaw)
	if err != nil {
		return "", "", fmt.Errorf("invalid signer_base_url: %w", err)
	}
	return control, signer, nil
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
