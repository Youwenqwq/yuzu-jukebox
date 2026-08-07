package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
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

type integrationInfoResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Active     bool   `json:"active"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
}

type createIntegrationRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type updateIntegrationRequest struct {
	Name   *string `json:"name"`
	Active *bool   `json:"active"`
}

var integrationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type integrationScopeResponse struct {
	IntegrationID string `json:"integration_id"`
	AdapterID     string `json:"adapter_id"`
	ScopeType     string `json:"scope_type"`
	ScopeID       string `json:"scope_id"`
	RoomID        string `json:"room_id"`
}

type integrationSubjectResponse struct {
	IntegrationID string `json:"integration_id"`
	AdapterID     string `json:"adapter_id"`
	ScopeType     string `json:"scope_type"`
	ScopeID       string `json:"scope_id"`
	SubjectID     string `json:"subject_id"`
	PrincipalID   string `json:"principal_id"`
}

type principalResponse struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Avatar string   `json:"avatar,omitempty"`
	Kind   string   `json:"kind"`
	Roles  []string `json:"roles"`
	Active bool     `json:"active"`
}

func (s *Server) listIntegrations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRoomAdmin); !ok {
		return
	}
	rows, err := s.st.ListIntegrations(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to list integrations")
		return
	}
	integrations := make([]integrationInfoResponse, len(rows))
	for i, row := range rows {
		integrations[i] = integrationResponse(row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"integrations": integrations})
}

func (s *Server) createIntegration(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	var body createIntegrationRequest
	if err := decodeIntegrationJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	body.Name = strings.TrimSpace(body.Name)
	if !integrationIDPattern.MatchString(body.ID) || body.Name == "" || len(body.Name) > 100 {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid integration id or name")
		return
	}
	token, tokenHash, err := auth.NewIntegrationToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to generate integration token")
		return
	}
	integration, err := s.st.CreateIntegration(r.Context(), body.ID, body.Name, tokenHash)
	if err != nil {
		writeErr(w, http.StatusConflict, "conflict", "integration already exists")
		return
	}
	s.audit(r.Context(), actor.ID, "integration.create", integration.ID, map[string]any{
		"name": integration.Name,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"integration": integrationResponse(integration),
		"token":       token,
	})
}

func (s *Server) updateIntegration(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	id := r.PathValue("id")
	current, err := s.st.GetIntegration(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "integration not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load integration")
		return
	}
	var body updateIntegrationRequest
	if err := decodeIntegrationJSON(r, &body); err != nil || (body.Name == nil && body.Active == nil) {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	name, active := current.Name, current.Active
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
		if name == "" || len(name) > 100 {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid integration name")
			return
		}
	}
	if body.Active != nil {
		active = *body.Active
	}
	updated, err := s.st.UpdateIntegration(r.Context(), id, name, active)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to update integration")
		return
	}
	if current.Active && !updated.Active {
		s.authm.RevokeIntegration(id)
	}
	s.audit(r.Context(), actor.ID, "integration.update", id, map[string]any{
		"name": updated.Name, "active": updated.Active,
	})
	writeJSON(w, http.StatusOK, map[string]any{"integration": integrationResponse(updated)})
}

func (s *Server) deleteIntegration(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.st.DeleteIntegration(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "integration not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to delete integration")
		return
	}
	s.authm.RevokeIntegration(id)
	s.audit(r.Context(), actor.ID, "integration.delete", id, map[string]any{})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) rotateIntegrationToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, auth.RoleRoomAdmin)
	if !ok {
		return
	}
	id := r.PathValue("id")
	token, tokenHash, err := auth.NewIntegrationToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to generate integration token")
		return
	}
	integration, err := s.st.RotateIntegrationToken(r.Context(), id, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "integration not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to rotate integration token")
		return
	}
	s.authm.RevokeIntegration(id)
	s.audit(r.Context(), actor.ID, "integration.token.rotate", id, map[string]any{})
	writeJSON(w, http.StatusOK, map[string]any{
		"integration": integrationResponse(integration),
		"token":       token,
	})
}

func integrationResponse(integration store.Integration) integrationInfoResponse {
	return integrationInfoResponse{
		ID: integration.ID, Name: integration.Name, Active: integration.Active,
		CreatedAt: integration.CreatedAt, UpdatedAt: integration.UpdatedAt,
		LastUsedAt: integration.LastUsedAt,
	}
}

func (s *Server) listIntegrationScopes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRoomAdmin); !ok {
		return
	}
	integrationID := r.PathValue("id")
	if !s.integrationConfigured(w, integrationID) {
		return
	}
	rows, err := s.st.ListExternalScopeRooms(r.Context(), integrationID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to list scope bindings")
		return
	}
	scopes := make([]integrationScopeResponse, len(rows))
	for i, row := range rows {
		scopes[i] = integrationScopeResponse{
			IntegrationID: row.IntegrationID,
			AdapterID:     row.AdapterID,
			ScopeType:     row.ScopeType,
			ScopeID:       row.ScopeID,
			RoomID:        row.RoomID,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopes": scopes})
}

func (s *Server) listIntegrationSubjects(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRoomAdmin); !ok {
		return
	}
	integrationID := r.PathValue("id")
	if !s.integrationConfigured(w, integrationID) {
		return
	}
	rows, err := s.st.ListExternalIdentityLinks(r.Context(), integrationID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to list subject links")
		return
	}
	subjects := make([]integrationSubjectResponse, len(rows))
	for i, row := range rows {
		subjects[i] = integrationSubjectResponse{
			IntegrationID: row.IntegrationID,
			AdapterID:     row.AdapterID,
			ScopeType:     row.ScopeType,
			ScopeID:       row.ScopeID,
			SubjectID:     row.SubjectID,
			PrincipalID:   row.PrincipalID,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subjects": subjects})
}

func (s *Server) listPrincipals(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRoomAdmin); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.st.ListPrincipals(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to list principals")
		return
	}
	principals := make([]principalResponse, len(rows))
	for i, row := range rows {
		principals[i] = principalResponse{
			ID: row.ID, Name: row.Name, Avatar: row.Avatar, Kind: row.Kind,
			Roles: row.Roles, Active: row.Active,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"principals": principals})
}

func (s *Server) listRoomGrants(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, auth.RoleRoomAdmin); !ok {
		return
	}
	roomID := r.PathValue("id")
	if _, err := s.st.GetRoom(r.Context(), roomID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "room not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load room")
		return
	}
	rows, err := s.st.ListRoomGrants(r.Context(), roomID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to list grants")
		return
	}
	grants := make([]roomGrantRequest, 0, len(rows))
	for _, row := range rows {
		if row.Capability != control.CapabilityController {
			continue
		}
		grants = append(grants, roomGrantRequest{
			RoomID: row.RoomID, PrincipalID: row.PrincipalID, Capability: row.Capability,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants})
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

	actorToken, expiresAt, err := s.authm.IssueIntegrationSession(
		identity,
		auth.IntegrationSessionSource{
			IntegrationID: integrationID,
			AdapterID:     body.AdapterID,
			ScopeType:     body.Scope.Type,
			ScopeID:       body.Scope.ID,
		},
		integrationActorSessionTTL,
	)
	if err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "principal is unavailable")
		return
	}
	credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	current, err := s.integrations.ValidateToken(r.Context(), credential)
	if err != nil || current.ID != integrationID {
		s.authm.Revoke(actorToken)
		writeErr(w, http.StatusUnauthorized, "unauthorized", "integration credential changed during actor resolve")
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
		s.audit(r.Context(), actor.ID, "integration.scope.unbind", integrationID, body)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if err := s.st.BindExternalScopeRoom(
		r.Context(), integrationID, body.AdapterID, body.ScopeType, body.ScopeID, body.RoomID,
	); err != nil {
		writeErr(w, http.StatusConflict, "conflict", "failed to bind scope")
		return
	}
	s.audit(r.Context(), actor.ID, "integration.scope.bind", integrationID, body)
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
		s.audit(r.Context(), actor.ID, "integration.subject.unlink", integrationID, body)
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
	s.audit(r.Context(), actor.ID, "integration.subject.link", integrationID, body)
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
	principal, err := s.st.GetPrincipal(r.Context(), body.PrincipalID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "principal not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load principal")
		return
	}
	// Guest 主身份是显示名的确定性派生（sha256("guest:"+name) 前 12 hex），
	// 任何人用同一名字登录即得到同一主身份；给 guest 授 controller 等于把房间
	// 交给任何知道该名字的人。拒绝新授，已存在的历史 grant 保持原语义。
	if body.Capability == control.CapabilityController && principal.Kind == "guest" {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"controller capability cannot be granted to a guest principal (name-derived identity is forgeable)")
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
		s.audit(r.Context(), actor.ID, "room.grant.revoke", body.RoomID, body)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if err := s.st.GrantRoomGrant(r.Context(), body.RoomID, body.PrincipalID, body.Capability); err != nil {
		writeErr(w, http.StatusConflict, "conflict", "failed to grant capability")
		return
	}
	s.audit(r.Context(), actor.ID, "room.grant.set", body.RoomID, body)
	writeJSON(w, http.StatusOK, map[string]any{"grant": body})
}

func (s *Server) audit(
	ctx context.Context,
	actorID, action, target string,
	detail any,
) {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return
	}
	_ = s.st.Audit(ctx, actorID, action, target, string(encoded))
}

func (s *Server) authenticateIntegration(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "integration bearer token required")
		return "", false
	}
	integration, err := s.integrations.ResolveToken(r.Context(), token)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid integration bearer token")
		return "", false
	}
	return integration.ID, true
}

func (s *Server) integrationConfigured(w http.ResponseWriter, integrationID string) bool {
	if _, err := s.st.GetIntegration(context.Background(), integrationID); errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "integration not found")
		return false
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to load integration")
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
