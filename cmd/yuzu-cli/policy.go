// policy.go — 房间治理策略命令实现。
package main

import (
	"context"
	"fmt"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func cmdPolicySet(ctx context.Context, roomID, policy string) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	if err := client.RESTUpdateRoomPolicy(ctx, *server, token, roomID, policy); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdPolicyShow(ctx context.Context, roomID string) error {
	token, err := client.RESTAuth(ctx, *server, *name, *password)
	if err != nil {
		return err
	}
	rooms, err := client.RESTListRooms(ctx, *server, token)
	if err != nil {
		return err
	}
	for _, r := range rooms {
		if r.ID == roomID {
			fmt.Println(string(r.Policy))
			return nil
		}
	}
	return fmt.Errorf("room %q not found", roomID)
}
