package httpapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

const (
	idempotencyPendingTTL  = 5 * time.Minute
	idempotencyCompleteTTL = 24 * time.Hour
	maxIdempotencyKeyBytes = 200
	maxIdempotencyBody     = 1 << 20
)

// idempotent protects Room REST mutations against external platform retries.
// Integration actor sessions must provide a key; normal clients may opt in.
func (s *Server) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, err := s.authenticate(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "valid session required")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if identity.IntegrationID != "" && key == "" {
			writeErr(w, http.StatusBadRequest, "idempotency_required", "Idempotency-Key is required for Integration actor writes")
			return
		}
		if key == "" {
			next(w, r)
			return
		}
		if len(key) > maxIdempotencyKeyBytes {
			writeErr(w, http.StatusBadRequest, "bad_request", "Idempotency-Key is too long")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxIdempotencyBody+1))
		if err != nil || len(body) > maxIdempotencyBody {
			writeErr(w, http.StatusBadRequest, "bad_request", "request body is too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		digest := sha256.Sum256(body)
		now := time.Now()
		record := store.IdempotencyRecord{
			ActorID: identity.ID, IntegrationID: identity.IntegrationID,
			Key: key, Method: r.Method, Path: r.URL.EscapedPath(),
			RequestHash: digest[:], ExpiresAt: now.Add(idempotencyPendingTTL).UnixMilli(),
		}
		current, created, err := s.st.BeginIdempotency(r.Context(), record)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to reserve idempotency key")
			return
		}
		if !created {
			if subtle.ConstantTimeCompare(current.RequestHash, record.RequestHash) != 1 {
				writeErr(w, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used with a different request body")
				return
			}
			if current.StatusCode == nil {
				writeErr(w, http.StatusConflict, "request_in_progress", "request with this Idempotency-Key is still in progress")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(*current.StatusCode)
			_, _ = w.Write(current.ResponseBody)
			return
		}

		captured := newBufferedResponse()
		next(captured, r)
		if captured.status >= http.StatusInternalServerError {
			_ = s.st.DeleteIdempotency(r.Context(), record.ActorID, record.IntegrationID, record.Key, record.Method, record.Path)
			captured.flush(w)
			return
		}
		record.StatusCode = &captured.status
		record.ResponseBody = captured.body.Bytes()
		record.ExpiresAt = now.Add(idempotencyCompleteTTL).UnixMilli()
		if err := s.st.CompleteIdempotency(r.Context(), record); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to persist idempotent response")
			return
		}
		captured.flush(w)
	}
}

type bufferedResponse struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	wroteHeader bool
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: make(http.Header), status: http.StatusOK}
}

func (w *bufferedResponse) Header() http.Header { return w.header }

func (w *bufferedResponse) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *bufferedResponse) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.body.Write(data)
}

func (w *bufferedResponse) flush(dst http.ResponseWriter) {
	for key, values := range w.header {
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	dst.WriteHeader(w.status)
	_, _ = dst.Write(w.body.Bytes())
}
