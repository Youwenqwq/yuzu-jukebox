// room.go — 房间管理、历史与统计命令实现。
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func cmdRooms(ctx context.Context) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	rooms, err := client.RESTListRooms(ctx, *server, token)
	if err != nil {
		return err
	}
	if len(rooms) == 0 {
		fmt.Println("(no rooms — create one with: yuzu-cli mkroom <id> <name>)")
		return nil
	}
	for _, r := range rooms {
		fmt.Printf("%-20s %s\n", r.ID, r.Name)
	}
	return nil
}

func cmdMkRoom(ctx context.Context, id, roomName string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTCreateRoom(ctx, *server, token, id, roomName, *roomPassword); err != nil {
		return err
	}
	fmt.Printf("room %q created (guest password: %s)\n", id, orNone(*roomPassword))
	return nil
}

func cmdHistory(ctx context.Context, roomID string, offset int) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	rows, err := client.RESTRoomHistory(ctx, *server, token, roomID, offset, *limit)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("（无记录）")
		return nil
	}
	for i, h := range rows {
		fmt.Printf("%3d. %s %-40s by %s (%s)\n", offset+i+1,
			time.UnixMilli(h.StartedAt).Format("01-02 15:04"), h.Title, h.RequestedBy, h.EndReason)
	}
	return nil
}

func cmdTop(ctx context.Context, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	stats, err := client.RESTRoomStats(ctx, *server, token, roomID, *limit)
	if err != nil {
		return err
	}
	if len(stats) == 0 {
		fmt.Println("（无记录）")
		return nil
	}
	for i, t := range stats {
		fmt.Printf("%3d. [%2d 次] %-40s 首播 %s  最近 %s\n", i+1, t.PlayCount, t.Title,
			time.UnixMilli(t.FirstPlayedAt).Format("2006-01-02"), time.UnixMilli(t.LastPlayedAt).Format("01-02 15:04"))
	}
	return nil
}
