package client

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

func TestRESTIssueExternalBindingCode(t *testing.T) {
	server := expectREST(t, restExpectation{
		method:     http.MethodPost,
		requestURI: "/api/v1/auth/external-binding-codes",
		token:      "oidc-session-token",
		body:       nil,
		response:   `{"code":"7K3M-9P2D-X4RT","expires_at":1720000600000}`,
	})

	issued, err := RESTIssueExternalBindingCode(context.Background(), server.URL, "oidc-session-token")
	if err != nil {
		t.Fatal(err)
	}
	if issued.Code != "7K3M-9P2D-X4RT" || issued.ExpiresAt != 1720000600000 {
		t.Fatalf("issued binding code = %#v", issued)
	}
}

func TestRESTRedeemExternalBindingCode(t *testing.T) {
	request := ExternalBindingRedeemRequest{
		Code: "7K3M-9P2D-X4RT",
		ExternalBindingTarget: ExternalBindingTarget{
			AdapterID: "astrbot",
			Scope: IntegrationActorScope{
				Type: "group",
				ID:   "123456",
			},
			Subject: ExternalBindingSubject{ID: "9988"},
		},
	}
	server := expectREST(t, restExpectation{
		method:     http.MethodPost,
		requestURI: "/api/v1/integrations/bindings/redeem",
		token:      "integration-token",
		body: map[string]any{
			"code":       "7K3M-9P2D-X4RT",
			"adapter_id": "astrbot",
			"scope":      map[string]string{"type": "group", "id": "123456"},
			"subject":    map[string]string{"id": "9988"},
		},
		response: `{"binding":{"integration_id":"bridge","adapter_id":"astrbot","scope_type":"group","scope_id":"123456","subject_id":"9988","principal_id":"principal-1"},"identity":{"id":"principal-1","name":"Yuzu User","kind":"oidc","roles":["listener","requester"]}}`,
	})

	got, err := RESTRedeemExternalBindingCode(context.Background(), server.URL, "integration-token", request)
	if err != nil {
		t.Fatal(err)
	}
	want := ExternalBindingRedeemResponse{
		Binding: ExternalBinding{
			IntegrationID: "bridge",
			AdapterID:     "astrbot",
			ScopeType:     "group",
			ScopeID:       "123456",
			SubjectID:     "9988",
			PrincipalID:   "principal-1",
		},
		Identity: Identity{
			ID:    "principal-1",
			Name:  "Yuzu User",
			Kind:  "oidc",
			Roles: []string{"listener", "requester"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redeemed binding = %#v, want %#v", got, want)
	}
}
