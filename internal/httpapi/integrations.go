package httpapi

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

const integrationActorSessionTTL = 5 * time.Minute

type integrationActorResolveRequest struct {
	AdapterID string `json:"adapter_id"`
	Scope     struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"scope"`
	Subject struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"subject"`
}

type integrationScopeRequest struct {
	AdapterID string `json:"adapter_id"`
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	RoomID    string `json:"room_id"`
}

type integrationSubjectRequest struct {
	AdapterID   string `json:"adapter_id"`
	ScopeType   string `json:"scope_type"`
	ScopeID     string `json:"scope_id"`
	SubjectID   string `json:"subject_id"`
	PrincipalID string `json:"principal_id"`
}

type roomGrantRequest struct {
	RoomID      string `json:"room_id"`
	PrincipalID string `json:"principal_id"`
	Capability  string `json:"capability"`
}

func (s *Server) resolveIntegrationActor(w http.ResponseWriter, r *http.Request) {
	integrationID, ok := s.authenticateIntegration(w, r)
	if !ok {
		return
	}
	var body integrationActorResolveRequest
	if err := decodeIntegrationJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	if anyBlank(body.AdapterID, body.Scope.Type, body.Scope.ID, body.Subject.ID, body.Subject.DisplayName) {
		writeErr(w, http.StatusBadRequest, "bad_request", "adapter_id, scope.type, scope.id, subject.id and subject.display_name are required")
		return
	}

	identity, ok := s.integrationActorIdentity(w, r, integrationID, body)
	if !ok {
		return
	}
	defaultRoomID, err := s.st.ResolveExternalScopeRoom(
		r.Context(), integrationID, body.AdapterID, body.Scope.Type, body.Scope.ID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		defaultRoomID = ""
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to resolve default room")
		return
	}

	actorToken, expiresAt, err := s.authm.IssueSessionWithTTL(identity, integrationActorSessionTTL)
	if err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "principal is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Identity      auth.Identity `json:"identity"`
		DefaultRoomID string        `json:"default_room_id,omitempty"`
		ActorToken    string        `json:"actor_token"`
		ExpiresAt     int64         `json:"expires_at"`
	}{
		Identity: identity, DefaultRoomID: defaultRoomID,
		ActorToken: actorToken, ExpiresAt: expiresAt,
	})
}

func (s *Server) integrationActorIdentity(
	w http.ResponseWriter,
	r *http.Request,
	integrationID string,
	body integrationActorResolveRequest,
) (auth.Identity, bool) {
	principalID, err := s.st.ResolveExternalIdentityLink(
		r.Context(), integrationID, body.AdapterID, body.Scope.Type, body.Scope.ID, body.Subject.ID,
	)
	if err == nil {
		principal, getErr := s.st.GetPrincipal(r.Context(), principalID)
		if errors.Is(getErr, sql.ErrNoRows) || (getErr == nil && !principal.Active) {
			writeErr(w, http.StatusForbidden, "forbidden", "linked principal is unavailable")
			return auth.Identity{}, false
		}
		if getErr != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to load linked principal")
			return auth.Identity{}, false
		}
		return identityFromStoredPrincipal(principal), true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to resolve external subject")
		return auth.Identity{}, false
	}

	identity := auth.Identity{
		ID: integrationGuestID(
			integrationID, body.AdapterID, body.Scope.Type, body.Scope.ID, body.Subject.ID,
		),
		Name:  body.Subject.DisplayName,
		Kind:  "guest",
		Roles: []string{auth.RoleListener, auth.RoleRequester},
	}
	if current, getErr := s.st.GetPrincipal(r.Context(), identity.ID); getErr == nil && !current.Active {
		writeErr(w, http.StatusForbidden, "forbidden", "guest principal is disabled")
		return auth.Identity{}, false
	} else if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load guest principal")
		return auth.Identity{}, false
	}
	if err := s.st.UpsertPrincipal(r.Context(), store.Principal{
		ID: identity.ID, Name: identity.Name, Kind: identity.Kind,
		Roles: identity.Roles, Active: true,
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to persist guest principal")
		return auth.Identity{}, false
	}
	return identity, true
}

func (s *Server) manageIntegrationScope(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	integrationID := r.PathValue("id")
	if !s.integrationConfigured(w, integrationID) {
		return
	}
	var body integrationScopeRequest
	if err := decodeIntegrationJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	if anyBlank(body.AdapterID, body.ScopeType, body.ScopeID, body.RoomID) {
		writeErr(w, http.StatusBadRequest, "bad_request", "adapter_id, scope_type, scope_id and room_id are required")
		return
	}
	if _, err := s.st.GetRoom(r.Context(), body.RoomID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room")
		return
	}

	if r.Method == http.MethodDelete {
		boundRoomID, err := s.st.ResolveExternalScopeRoom(
			r.Context(), integrationID, body.AdapterID, body.ScopeType, body.ScopeID,
		)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "scope binding not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to load scope binding")
			return
		}
		if boundRoomID != body.RoomID {
			writeErr(w, http.StatusConflict, "conflict", "room_id does not match the current binding")
			return
		}
		if err := s.st.RemoveExternalScopeRoom(
			r.Context(), integrationID, body.AdapterID, body.ScopeType, body.ScopeID,
		); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to remove scope binding")
			return
		}
		s.st.Audit(r.Context(), actor.ID, "integration.scope.unbind", integrationID, "{}")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if err := s.st.BindExternalScopeRoom(
		r.Context(), integrationID, body.AdapterID, body.ScopeType, body.ScopeID, body.RoomID,
	); err != nil {
		writeErr(w, http.StatusConflict, "conflict", "failed to bind scope")
		return
	}
	s.st.Audit(r.Context(), actor.ID, "integration.scope.bind", integrationID, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"scope": map[string]any{
		"integration_id": integrationID, "adapter_id": body.AdapterID,
		"scope_type": body.ScopeType, "scope_id": body.ScopeID, "room_id": body.RoomID,
	}})
}

func (s *Server) manageIntegrationSubject(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	integrationID := r.PathValue("id")
	if !s.integrationConfigured(w, integrationID) {
		return
	}
	var body integrationSubjectRequest
	if err := decodeIntegrationJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	if anyBlank(body.AdapterID, body.ScopeType, body.ScopeID, body.SubjectID, body.PrincipalID) {
		writeErr(w, http.StatusBadRequest, "bad_request", "adapter_id, scope_type, scope_id, subject_id and principal_id are required")
		return
	}
	if _, err := s.st.GetPrincipal(r.Context(), body.PrincipalID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "principal not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load principal")
		return
	}

	if r.Method == http.MethodDelete {
		linkedPrincipalID, err := s.st.ResolveExternalIdentityLink(
			r.Context(), integrationID, body.AdapterID, body.ScopeType, body.ScopeID, body.SubjectID,
		)
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not_found", "subject link not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to load subject link")
			return
		}
		if linkedPrincipalID != body.PrincipalID {
			writeErr(w, http.StatusConflict, "conflict", "principal_id does not match the current link")
			return
		}
		if err := s.st.RemoveExternalIdentityLink(
			r.Context(), integrationID, body.AdapterID, body.ScopeType, body.ScopeID, body.SubjectID,
		); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to remove subject link")
			return
		}
		s.st.Audit(r.Context(), actor.ID, "integration.subject.unlink", integrationID, "{}")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if err := s.st.UpsertExternalIdentityLink(
		r.Context(), integrationID, body.AdapterID, body.ScopeType, body.ScopeID,
		body.SubjectID, body.PrincipalID,
	); err != nil {
		writeErr(w, http.StatusConflict, "conflict", "failed to link subject")
		return
	}
	s.st.Audit(r.Context(), actor.ID, "integration.subject.link", integrationID, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"subject": map[string]any{
		"integration_id": integrationID, "adapter_id": body.AdapterID,
		"scope_type": body.ScopeType, "scope_id": body.ScopeID,
		"subject_id": body.SubjectID, "principal_id": body.PrincipalID,
	}})
}

func (s *Server) manageRoomGrant(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	var body roomGrantRequest
	if err := decodeIntegrationJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	if anyBlank(body.RoomID, body.PrincipalID, body.Capability) {
		writeErr(w, http.StatusBadRequest, "bad_request", "room_id, principal_id and capability are required")
		return
	}
	if body.Capability != control.CapabilityController {
		writeErr(w, http.StatusBadRequest, "bad_request", "unsupported capability")
		return
	}
	if body.RoomID != r.PathValue("id") || body.PrincipalID != r.PathValue("principal_id") {
		writeErr(w, http.StatusConflict, "conflict", "request body does not match path")
		return
	}
	if _, err := s.st.GetRoom(r.Context(), body.RoomID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room")
		return
	}
	if _, err := s.st.GetPrincipal(r.Context(), body.PrincipalID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "principal not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load principal")
		return
	}

	if r.Method == http.MethodDelete {
		granted, err := s.st.HasRoomGrant(r.Context(), body.RoomID, body.PrincipalID, body.Capability)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to load grant")
			return
		}
		if !granted {
			writeErr(w, http.StatusNotFound, "not_found", "grant not found")
			return
		}
		if err := s.st.RevokeRoomGrant(r.Context(), body.RoomID, body.PrincipalID, body.Capability); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "failed to revoke grant")
			return
		}
		s.st.Audit(r.Context(), actor.ID, "room.grant.revoke", body.RoomID, "{}")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if err := s.st.GrantRoomGrant(r.Context(), body.RoomID, body.PrincipalID, body.Capability); err != nil {
		writeErr(w, http.StatusConflict, "conflict", "failed to grant capability")
		return
	}
	s.st.Audit(r.Context(), actor.ID, "room.grant.set", body.RoomID, "{}")
	writeJSON(w, http.StatusOK, map[string]any{"grant": body})
}

func (s *Server) authenticateIntegration(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "integration bearer token required")
		return "", false
	}
	integrationID, ok := s.integrations.ResolveToken(token)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid integration bearer token")
		return "", false
	}
	return integrationID, true
}

func (s *Server) integrationConfigured(w http.ResponseWriter, integrationID string) bool {
	if !s.integrations.Contains(integrationID) {
		writeErr(w, http.StatusNotFound, "not_found", "integration not configured")
		return false
	}
	return true
}

func decodeIntegrationJSON(r *http.Request, dst any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one json object")
	}
	return nil
}

func anyBlank(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}

func identityFromStoredPrincipal(principal store.Principal) auth.Identity {
	return auth.Identity{
		ID: principal.ID, Name: principal.Name, Kind: principal.Kind,
		Roles: principal.Roles, OIDCSubject: principal.OIDCSubject,
	}
}

func integrationGuestID(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return "ig_" + hex.EncodeToString(digest[:])
}
