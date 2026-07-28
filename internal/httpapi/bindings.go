package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
)

type externalBindingRedeemRequest struct {
	Code      string `json:"code"`
	AdapterID string `json:"adapter_id"`
	Scope     struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"scope"`
	Subject struct {
		ID string `json:"id"`
	} `json:"subject"`
}

type externalBindingResponse struct {
	IntegrationID string `json:"integration_id"`
	AdapterID     string `json:"adapter_id"`
	ScopeType     string `json:"scope_type"`
	ScopeID       string `json:"scope_id"`
	SubjectID     string `json:"subject_id"`
	PrincipalID   string `json:"principal_id"`
}

func (s *Server) issueExternalBindingCode(w http.ResponseWriter, r *http.Request) {
	identity, err := s.authenticate(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "login first")
		return
	}
	if identity.IntegrationID != "" || identity.OIDCSubject == "" {
		writeErr(w, http.StatusForbidden, "forbidden", "OIDC login required")
		return
	}
	issued, err := s.bindings.Issue(r.Context(), identity)
	if errors.Is(err, auth.ErrBindingRequiresOIDC) || errors.Is(err, auth.ErrBindingPrincipalUnavailable) {
		writeErr(w, http.StatusForbidden, "forbidden", "OIDC principal is unavailable")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to issue binding code")
		return
	}
	s.audit(r.Context(), identity.ID, "identity.binding_code.issue", identity.ID, map[string]any{
		"expires_at": issued.ExpiresAt,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"code": issued.Code, "expires_at": issued.ExpiresAt,
	})
}

func (s *Server) redeemExternalBindingCode(w http.ResponseWriter, r *http.Request) {
	integrationID, ok := s.authenticateIntegration(w, r)
	if !ok {
		return
	}
	var body externalBindingRedeemRequest
	if err := decodeIntegrationJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	if anyBlank(body.Code, body.AdapterID, body.Scope.Type, body.Scope.ID, body.Subject.ID) {
		writeErr(w, http.StatusBadRequest, "bad_request", "code, adapter_id, scope.type, scope.id and subject.id are required")
		return
	}

	credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	target := auth.ExternalBindingTarget{
		IntegrationID: integrationID,
		AdapterID:     body.AdapterID,
		ScopeType:     body.Scope.Type,
		ScopeID:       body.Scope.ID,
		SubjectID:     body.Subject.ID,
	}
	redemption, err := s.bindings.Redeem(r.Context(), credential, body.Code, target)
	switch {
	case errors.Is(err, auth.ErrBindingCodeInvalid):
		writeErr(w, http.StatusBadRequest, "invalid_binding_code", "binding code is invalid, expired or already consumed")
		return
	case errors.Is(err, auth.ErrBindingConflict):
		writeErr(w, http.StatusConflict, "conflict", "external subject is already linked to another principal")
		return
	case errors.Is(err, auth.ErrBindingPrincipalUnavailable):
		writeErr(w, http.StatusForbidden, "forbidden", "OIDC principal is unavailable")
		return
	case errors.Is(err, auth.ErrBindingIntegrationUnavailable):
		writeErr(w, http.StatusUnauthorized, "unauthorized", "integration credential changed during binding")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", "failed to redeem binding code")
		return
	}

	if redemption.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	} else {
		s.audit(r.Context(), redemption.Identity.ID, "identity.external.bind", integrationID, map[string]any{
			"adapter_id": target.AdapterID,
			"scope_type": target.ScopeType,
			"scope_id":   target.ScopeID,
			"subject_id": target.SubjectID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"binding": externalBindingResponse{
			IntegrationID: integrationID,
			AdapterID:     target.AdapterID,
			ScopeType:     target.ScopeType,
			ScopeID:       target.ScopeID,
			SubjectID:     target.SubjectID,
			PrincipalID:   redemption.Identity.ID,
		},
		"identity": redemption.Identity,
	})
}
