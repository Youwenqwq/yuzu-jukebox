// integration.go — Integration、Principal 与 Room controller 管理命令实现。
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func cmdIntegrations(ctx context.Context) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	integrations, err := client.RESTListIntegrations(ctx, *server, token)
	if err != nil {
		return err
	}
	for _, integration := range integrations {
		fmt.Println(integration.ID)
	}
	return nil
}

func cmdIntegrationScopes(ctx context.Context, integrationID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	bindings, err := client.RESTListIntegrationScopes(ctx, *server, token, integrationID)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		fmt.Printf("%-16s %-12s %-24s %s\n", binding.AdapterID, binding.ScopeType, binding.ScopeID, binding.RoomID)
	}
	return nil
}

func cmdIntegrationScopeBind(ctx context.Context, integrationID, adapterID, scopeType, scopeID, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	binding := client.IntegrationScopeBinding{
		AdapterID: adapterID,
		ScopeType: scopeType,
		ScopeID:   scopeID,
		RoomID:    roomID,
	}
	if err := client.RESTBindIntegrationScope(ctx, *server, token, integrationID, binding); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdIntegrationScopeUnbind(ctx context.Context, integrationID, adapterID, scopeType, scopeID, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	binding := client.IntegrationScopeBinding{
		AdapterID: adapterID,
		ScopeType: scopeType,
		ScopeID:   scopeID,
		RoomID:    roomID,
	}
	if err := client.RESTUnbindIntegrationScope(ctx, *server, token, integrationID, binding); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdIntegrationSubjects(ctx context.Context, integrationID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	links, err := client.RESTListIntegrationSubjects(ctx, *server, token, integrationID)
	if err != nil {
		return err
	}
	for _, link := range links {
		fmt.Printf("%-16s %-12s %-24s %-24s %s\n", link.AdapterID, link.ScopeType, link.ScopeID, link.SubjectID, link.PrincipalID)
	}
	return nil
}

func cmdIntegrationSubjectLink(ctx context.Context, integrationID, adapterID, scopeType, scopeID, subjectID, principalID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	link := client.IntegrationSubjectLink{
		AdapterID:   adapterID,
		ScopeType:   scopeType,
		ScopeID:     scopeID,
		SubjectID:   subjectID,
		PrincipalID: principalID,
	}
	if err := client.RESTLinkIntegrationSubject(ctx, *server, token, integrationID, link); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdIntegrationSubjectUnlink(ctx context.Context, integrationID, adapterID, scopeType, scopeID, subjectID, principalID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	link := client.IntegrationSubjectLink{
		AdapterID:   adapterID,
		ScopeType:   scopeType,
		ScopeID:     scopeID,
		SubjectID:   subjectID,
		PrincipalID: principalID,
	}
	if err := client.RESTUnlinkIntegrationSubject(ctx, *server, token, integrationID, link); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdPrincipals(ctx context.Context, query string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	principals, err := client.RESTListPrincipals(ctx, *server, token, query, *limit)
	if err != nil {
		return err
	}
	for _, principal := range principals {
		status := "inactive"
		if principal.Active {
			status = "active"
		}
		fmt.Printf("%-24s %-24s %-12s %-16s %s\n", principal.ID, principal.Name, principal.Kind, strings.Join(principal.Roles, ","), status)
	}
	return nil
}

func cmdRoomControllers(ctx context.Context, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	grants, err := client.RESTListRoomGrants(ctx, *server, token, roomID)
	if err != nil {
		return err
	}
	for _, grant := range grants {
		fmt.Printf("%-24s %s\n", grant.PrincipalID, grant.Capability)
	}
	return nil
}

func cmdRoomControllerGrant(ctx context.Context, roomID, principalID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTGrantRoomController(ctx, *server, token, roomID, principalID); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdRoomControllerRevoke(ctx context.Context, roomID, principalID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTRevokeRoomController(ctx, *server, token, roomID, principalID); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}
