package httpapi

import (
	"crypto/subtle"
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
)

const distributionBodyLimit = 32 << 10

func (s *Server) distributionIntrospect(w http.ResponseWriter, r *http.Request) {
	if !s.requireDistributionToken(w, r, s.distributionEdge) {
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
	if err := s.distribution.Request(r.Context(), ref); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "request distribution")
		return
	}
	candidate, ready, err := s.distribution.Candidate(r.Context(), ref)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "resolve distribution candidate")
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{
		"valid":     true,
		"backend":   s.distribution.Backend(),
		"track_ref": ref.String(),
		"ready":     ready,
		"candidate": optionalCandidate(candidate, ready),
	})
}

func optionalCandidate(candidate distribution.Candidate, ready bool) any {
	if !ready {
		return nil
	}
	return candidate
}

func (s *Server) distributionClaim(w http.ResponseWriter, r *http.Request) {
	if !s.requireDistributionToken(w, r, s.distributionPublisher) {
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
	lease, err := s.distribution.Claim(
		r.Context(), body.Owner, time.Duration(body.LeaseSeconds)*time.Second,
	)
	if errors.Is(err, distribution.ErrNoWork) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "claim distribution lease")
		return
	}
	writeDistributionJSON(w, http.StatusCreated, map[string]any{
		"lease": lease,
		"source_url": fmt.Sprintf(
			"/internal/v1/distribution/leases/%s/source", lease.ID,
		),
	})
}

func (s *Server) distributionSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireDistributionToken(w, r, s.distributionPublisher) {
		return
	}
	lease, err := s.distribution.Lease(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDistributionLeaseError(w, err)
		return
	}
	ref := provider.TrackRef(lease.TrackRef)
	f, err := s.cache.Open(r.Context(), ref)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
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
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func (s *Server) distributionComplete(w http.ResponseWriter, r *http.Request) {
	if !s.requireDistributionToken(w, r, s.distributionPublisher) {
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
	lease, err := s.distribution.Lease(r.Context(), r.PathValue("id"))
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
	if err := s.distribution.Complete(r.Context(), lease.ID, body.Owner, candidate); err != nil {
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
	if !s.requireDistributionToken(w, r, s.distributionPublisher) {
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
	err := s.distribution.Fail(
		r.Context(), r.PathValue("id"), body.Owner, body.Error,
		time.Duration(body.RetryAfterSeconds)*time.Second,
	)
	if err != nil {
		writeDistributionLeaseError(w, err)
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{"failed": true})
}

func (s *Server) distributionEvent(w http.ResponseWriter, r *http.Request) {
	if !s.requireDistributionToken(w, r, s.distributionEdge) {
		return
	}
	var body struct {
		TrackRef   string `json:"track_ref"`
		Ticket     string `json:"ticket"`
		Kind       string `json:"kind"`
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
		metrics["edge_fallback"] = 1
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown distribution event")
		return
	}
	for name, delta := range metrics {
		if err := s.distribution.AddMetric(r.Context(), name, delta); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "record distribution event")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) distributionMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.requireDistributionToken(w, r, s.distributionPublisher) {
		return
	}
	metrics, status, err := s.distribution.Metrics(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "read distribution metrics")
		return
	}
	writeDistributionJSON(w, http.StatusOK, map[string]any{
		"backend": s.distribution.Backend(), "status": status, "counters": metrics,
	})
}

func (s *Server) requireDistributionToken(w http.ResponseWriter, r *http.Request, expected string) bool {
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid distribution credential")
		return false
	}
	return true
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
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, value)
}

func writeDistributionLeaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, distribution.ErrInvalidLease):
		writeErr(w, http.StatusConflict, "lease_invalid", "distribution lease is not active")
	case errors.Is(err, distribution.ErrExpiredLease):
		writeErr(w, http.StatusConflict, "lease_expired", "distribution lease expired")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
