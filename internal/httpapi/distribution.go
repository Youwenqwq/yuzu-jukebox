package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/distribution"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

const distributionBodyLimit = 32 << 10

func (s *Server) distributionIntrospect(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationDelivery(w, r)
	if !ok {
		return
	}
	var body struct {
		TrackRef string `json:"track_ref"`
		Ticket   string `json:"ticket"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	ref := provider.TrackRef(strings.TrimSpace(body.TrackRef))
	if _, _, err := ref.Split(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid track_ref")
		return
	}
	if err := s.authm.ValidateTicket(body.Ticket, ref.String()); err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or expired ticket")
		return
	}
	if !acceleration.Enabled {
		writeDistributionJSON(w, http.StatusOK, map[string]any{
			"valid": true, "enabled": false, "acceleration_id": acceleration.ID,
			"track_ref": ref.String(), "ready": false, "candidate": nil,
			"fallback_reason": "acceleration_disabled",
		})
		return
	}
	if err := s.distribution.Request(r.Context(), acceleration.ID, ref); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "request distribution")
		return
	}
	candidate, ready, err := s.distribution.Candidate(r.Context(), acceleration.ID, ref)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "resolve distribution candidate")
		return
	}
	if ready {
		_ = s.st.TouchAccelerationObject(r.Context(), acceleration.ID, ref.String(), time.Now().UnixMilli())
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{
		"valid": true, "enabled": true, "acceleration_id": acceleration.ID,
		"track_ref": ref.String(), "ready": ready,
		"candidate":       optionalCandidate(candidate, ready),
		"fallback_reason": fallbackReason(ready, "candidate_not_ready"),
	})
}

func fallbackReason(ready bool, reason string) string {
	if ready {
		return ""
	}
	return reason
}

func optionalCandidate(candidate distribution.Candidate, ready bool) any {
	if !ready {
		return nil
	}
	return candidate
}

func (s *Server) distributionPublisherConfig(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{
		"acceleration_id": acceleration.ID, "enabled": acceleration.Enabled,
		"kind": acceleration.Kind, "backend_base_url": acceleration.BackendBaseURL,
		"backend_token":                  acceleration.BackendToken,
		"lease_ttl_seconds":              acceleration.LeaseTTLSeconds,
		"upload_rate_bytes_per_second":   acceleration.UploadRateBytesPerSecond,
		"max_object_bytes":               acceleration.MaxObjectBytes,
		"storage_budget_bytes":           acceleration.StorageBudgetBytes,
		"storage_high_watermark_percent": acceleration.StorageHighWatermarkPercent,
		"storage_low_watermark_percent":  acceleration.StorageLowWatermarkPercent,
	})
}

func (s *Server) distributionHeartbeat(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner          string   `json:"owner"`
		Version        string   `json:"version"`
		State          string   `json:"state"`
		LeaseID        string   `json:"lease_id"`
		TrackRef       string   `json:"track_ref"`
		Capabilities   []string `json:"capabilities"`
		BackendHealthy bool     `json:"backend_healthy"`
		LastError      string   `json:"last_error"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	if err := s.distribution.Heartbeat(r.Context(), acceleration.ID, body.Owner,
		body.Version, body.State, body.LeaseID, body.TrackRef, body.Capabilities,
		body.BackendHealthy, body.LastError); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) distributionClaim(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner        string `json:"owner"`
		LeaseSeconds int    `json:"lease_seconds"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Owner) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "owner is required")
		return
	}
	lease, err := s.distribution.Claim(r.Context(), acceleration, body.Owner,
		time.Duration(body.LeaseSeconds)*time.Second)
	if errors.Is(err, distribution.ErrNoWork) || errors.Is(err, distribution.ErrAccelerationDisabled) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "claim distribution lease")
		return
	}
	writeDistributionJSON(w, http.StatusCreated, map[string]any{
		"lease":      lease,
		"source_url": fmt.Sprintf("/internal/v1/accelerations/leases/%s/source", lease.ID),
	})
}
func (s *Server) distributionLeaseStatus(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	lease, err := s.distribution.LeaseStatus(r.Context(), acceleration.ID, r.PathValue("id"))
	if err != nil {
		writeDistributionLeaseError(w, err)
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{"lease": lease})
}

func (s *Server) distributionSource(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	lease, err := s.distribution.Lease(r.Context(), acceleration.ID, r.PathValue("id"))
	if err != nil {
		writeDistributionLeaseError(w, err)
		return
	}
	ref := provider.TrackRef(lease.TrackRef)
	file, err := s.cache.Open(r.Context(), ref)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "stat distribution source")
		return
	}
	if contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(info.Name()))); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Yuzu-Distribution-Lease", lease.ID)
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *Server) distributionProgress(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner       string `json:"owner"`
		Phase       string `json:"phase"`
		SourceBytes int64  `json:"source_bytes"`
		UploadBytes int64  `json:"upload_bytes"`
		TotalBytes  int64  `json:"total_bytes"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	lease, err := s.distribution.Progress(r.Context(), acceleration, r.PathValue("id"),
		body.Owner, body.Phase, body.SourceBytes, body.UploadBytes, body.TotalBytes)
	if err != nil {
		writeDistributionLeaseError(w, err)
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{"lease": lease})
}

func (s *Server) distributionComplete(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner          string `json:"owner"`
		ContentVersion string `json:"content_version"`
		Locator        string `json:"locator"`
		Layout         string `json:"layout"`
		SizeBytes      int64  `json:"size_bytes"`
		ContentType    string `json:"content_type"`
		ETag           string `json:"etag"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	lease, err := s.distribution.Lease(r.Context(), acceleration.ID, r.PathValue("id"))
	if err != nil {
		writeDistributionLeaseError(w, err)
		return
	}
	if lease.Owner != body.Owner {
		writeErr(w, http.StatusConflict, "lease_invalid", "lease owner mismatch")
		return
	}
	candidate := distribution.Candidate{
		TrackRef: lease.TrackRef, ContentVersion: body.ContentVersion,
		Locator: body.Locator, Layout: body.Layout, SizeBytes: body.SizeBytes,
		ContentType: body.ContentType, ETag: body.ETag,
	}
	if err := s.distribution.Complete(r.Context(), acceleration.ID, lease.ID,
		body.Owner, candidate); err != nil {
		if errors.Is(err, distribution.ErrInvalidLease) || errors.Is(err, distribution.ErrExpiredLease) {
			writeDistributionLeaseError(w, err)
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{"ready": true})
}

func (s *Server) distributionFail(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner             string `json:"owner"`
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	if body.RetryAfterSeconds < 0 || body.RetryAfterSeconds > 7*24*60*60 {
		writeErr(w, http.StatusBadRequest, "bad_request", "retry_after_seconds out of range")
		return
	}
	err := s.distribution.Fail(r.Context(), acceleration.ID, r.PathValue("id"),
		body.Owner, body.Error, time.Duration(body.RetryAfterSeconds)*time.Second)
	if err != nil {
		writeDistributionLeaseError(w, err)
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{"failed": true})
}
func (s *Server) distributionCancel(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	var body struct {
		Owner string `json:"owner"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	err := s.distribution.CompleteCancellation(r.Context(), acceleration.ID,
		r.PathValue("id"), body.Owner)
	if err != nil {
		writeDistributionLeaseError(w, err)
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{"canceled": true})
}

func (s *Server) distributionEvent(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationDelivery(w, r)
	if !ok {
		return
	}
	var body struct {
		TrackRef   string `json:"track_ref"`
		Ticket     string `json:"ticket"`
		Kind       string `json:"kind"`
		Reason     string `json:"reason"`
		DurationMS int64  `json:"duration_ms"`
		Bytes      int64  `json:"bytes"`
	}
	if !decodeDistributionJSON(w, r, &body) {
		return
	}
	ref := provider.TrackRef(strings.TrimSpace(body.TrackRef))
	if _, _, err := ref.Split(); err != nil || s.authm.ValidateTicket(body.Ticket, ref.String()) != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or expired ticket")
		return
	}
	metrics := map[string]int64{}
	switch body.Kind {
	case "blob_served":
		metrics["edge_blob_served"] = 1
		metrics["blob_response_ms_total"] = max(body.DurationMS, 0)
		metrics["blob_response_samples"] = 1
		metrics["blob_bytes_served"] = max(body.Bytes, 0)
	case "fallback":
		if !validFallbackReason(body.Reason) {
			writeErr(w, http.StatusBadRequest, "bad_request", "unknown fallback reason")
			return
		}
		metrics["edge_fallback"] = 1
		metrics["edge_fallback_"+body.Reason] = 1
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown distribution event")
		return
	}
	for name, delta := range metrics {
		if err := s.distribution.AddMetric(r.Context(), acceleration.ID, name, delta); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "record distribution event")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func validFallbackReason(reason string) bool {
	switch reason {
	case "acceleration_disabled", "candidate_not_ready", "backend_unavailable",
		"blob_http_status", "blob_fetch_error", "control_unavailable":
		return true
	default:
		return false
	}
}

func (s *Server) distributionMetrics(w http.ResponseWriter, r *http.Request) {
	acceleration, ok := s.authenticateAccelerationPublisher(w, r)
	if !ok {
		return
	}
	metrics, status, err := s.distribution.Metrics(r.Context(), acceleration.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "read distribution metrics")
		return
	}
	rolling, err := s.distribution.Metrics24Hours(r.Context(), acceleration.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "read rolling distribution metrics")
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{
		"acceleration_id": acceleration.ID, "status": status,
		"counters": metrics, "last_24_hours": rolling,
	})
}

func (s *Server) authenticateAccelerationPublisher(w http.ResponseWriter, r *http.Request) (store.Acceleration, bool) {
	return s.authenticateAccelerationCredential(w, r, s.accelerationRegistry.ResolvePublisher)
}

func (s *Server) authenticateAccelerationDelivery(w http.ResponseWriter, r *http.Request) (store.Acceleration, bool) {
	return s.authenticateAccelerationCredential(w, r, s.accelerationRegistry.ResolveDelivery)
}

func (s *Server) authenticateAccelerationCredential(
	w http.ResponseWriter,
	r *http.Request,
	resolve func(context.Context, string) (store.Acceleration, error),
) (store.Acceleration, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid acceleration credential")
		return store.Acceleration{}, false
	}
	acceleration, err := resolve(r.Context(), strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid acceleration credential")
		return store.Acceleration{}, false
	}
	return acceleration, true
}

func decodeDistributionJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, distributionBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return false
	}
	return true
}

func writeDistributionJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeDistributionLeaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, distribution.ErrInvalidLease), errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "lease_not_found", "distribution lease not found")
	case errors.Is(err, distribution.ErrExpiredLease):
		writeErr(w, http.StatusConflict, "lease_expired", "distribution lease expired")
	case errors.Is(err, distribution.ErrCancellationRequested):
		writeErr(w, http.StatusConflict, "cancellation_requested", "distribution cancellation requested")
	case errors.Is(err, distribution.ErrStaleProgress):
		writeErr(w, http.StatusConflict, "progress_stale", "distribution progress moved backwards")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", "distribution lease operation failed")
	}
}
