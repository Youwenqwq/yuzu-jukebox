package client

import (
	"context"
	"net/http"
	"testing"
)

func TestRESTResolveIntegrationActor(t *testing.T) {
	request := IntegrationActorResolveRequest{
		AdapterID: "onebot/v11",
		Scope: IntegrationActorScope{
			Type: "group",
			ID:   "group/42",
		},
		Subject: IntegrationActorSubject{
			ID:          "user/7",
			DisplayName: "Yuzu User",
		},
	}
	server := expectREST(t, restExpectation{
		method:     http.MethodPost,
		requestURI: "/api/v1/integrations/actors/resolve",
		token:      "integration-token",
		body:       request,
		response:   `{"identity":{"id":"principal-7","name":"Yuzu User","kind":"external","roles":["listener","requester"]},"default_room_id":"room-42","actor_token":"actor-token","expires_at":1785067500000}`,
	})

	resolved, err := RESTResolveIntegrationActor(context.Background(), server.URL, "integration-token", request)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Identity.ID != "principal-7" || resolved.Identity.Kind != "external" {
		t.Fatalf("identity = %#v", resolved.Identity)
	}
	if resolved.DefaultRoomID != "room-42" {
		t.Fatalf("default room ID = %q, want room-42", resolved.DefaultRoomID)
	}
	if resolved.ActorToken != "actor-token" {
		t.Fatalf("actor token = %q, want actor-token", resolved.ActorToken)
	}
	if resolved.ExpiresAt != 1785067500000 {
		t.Fatalf("expires at = %d", resolved.ExpiresAt)
	}
}

func TestRESTIntegrationScopeBinding(t *testing.T) {
	binding := IntegrationScopeBinding{
		AdapterID: "onebot/v11",
		ScopeType: "group",
		ScopeID:   "group/42",
		RoomID:    "room/42",
	}
	tests := []struct {
		name   string
		method string
		call   func(string) error
	}{
		{
			name:   "bind",
			method: http.MethodPut,
			call: func(server string) error {
				return RESTBindIntegrationScope(context.Background(), server, "actor-token", "integration/a b", binding)
			},
		},
		{
			name:   "unbind",
			method: http.MethodDelete,
			call: func(server string) error {
				return RESTUnbindIntegrationScope(context.Background(), server, "actor-token", "integration/a b", binding)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := expectREST(t, restExpectation{
				method:     tt.method,
				requestURI: "/api/v1/integrations/integration%2Fa%20b/scopes",
				token:      "actor-token",
				body:       binding,
				response:   `{}`,
			})
			if err := tt.call(server.URL); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRESTIntegrationSubjectLink(t *testing.T) {
	link := IntegrationSubjectLink{
		AdapterID:   "onebot/v11",
		ScopeType:   "group",
		ScopeID:     "group/42",
		SubjectID:   "user/7",
		PrincipalID: "principal/7",
	}
	tests := []struct {
		name   string
		method string
		call   func(string) error
	}{
		{
			name:   "link",
			method: http.MethodPut,
			call: func(server string) error {
				return RESTLinkIntegrationSubject(context.Background(), server, "actor-token", "integration/a b", link)
			},
		},
		{
			name:   "unlink",
			method: http.MethodDelete,
			call: func(server string) error {
				return RESTUnlinkIntegrationSubject(context.Background(), server, "actor-token", "integration/a b", link)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := expectREST(t, restExpectation{
				method:     tt.method,
				requestURI: "/api/v1/integrations/integration%2Fa%20b/subjects",
				token:      "actor-token",
				body:       link,
				response:   `{}`,
			})
			if err := tt.call(server.URL); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRESTRoomControllerGrant(t *testing.T) {
	body := RoomControllerGrant{
		RoomID:      "room/a b",
		PrincipalID: "principal/7 x",
		Capability:  "controller",
	}
	tests := []struct {
		name   string
		method string
		call   func(string) error
	}{
		{
			name:   "grant",
			method: http.MethodPut,
			call: func(server string) error {
				return RESTGrantRoomController(context.Background(), server, "actor-token", "room/a b", "principal/7 x")
			},
		},
		{
			name:   "revoke",
			method: http.MethodDelete,
			call: func(server string) error {
				return RESTRevokeRoomController(context.Background(), server, "actor-token", "room/a b", "principal/7 x")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := expectREST(t, restExpectation{
				method:     tt.method,
				requestURI: "/api/v1/rooms/room%2Fa%20b/grants/principal%2F7%20x",
				token:      "actor-token",
				body:       body,
				response:   `{}`,
			})
			if err := tt.call(server.URL); err != nil {
				t.Fatal(err)
			}
		})
	}
}
