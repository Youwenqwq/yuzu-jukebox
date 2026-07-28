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
		period := ""
		if r.GuestAccess.CodePeriodSeconds > 0 {
			period = " " + (time.Duration(r.GuestAccess.CodePeriodSeconds) * time.Second).String()
		}
		fmt.Printf("%-20s %-16s%s %s\n", r.ID, r.GuestAccess.Mode, period, r.Name)
	}
	return nil
}

func cmdMkRoom(ctx context.Context, id, roomName string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	request := client.RoomCreateRequest{
		ID: id, Name: roomName,
		GuestPassword: *roomPassword, GuestAccessMode: *roomAccessMode,
		GuestCodePeriodSeconds: int64(roomCodePeriod.Seconds()),
	}
	if err := client.RESTCreateRoom(ctx, *server, token, request); err != nil {
		return err
	}
	mode := *roomAccessMode
	if mode == "" {
		mode = "open"
		if *roomPassword != "" {
			mode = "static_password"
		}
	}
	fmt.Printf("room %q created (guest access: %s)\n", id, mode)
	return nil
}

func cmdRoomAccess(ctx context.Context, roomID, mode string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	update := client.RoomAccessUpdate{Mode: mode}
	switch mode {
	case "static_password":
		update.GuestPassword = roomPassword
	case "rotating_code":
		period := int64(roomCodePeriod.Seconds())
		update.CodePeriodSeconds = &period
	case "open":
	default:
		return fmt.Errorf("invalid room access mode %q", mode)
	}
	access, err := client.RESTUpdateRoomAccess(ctx, *server, token, roomID, update)
	if err != nil {
		return err
	}
	fmt.Printf("room %q guest access: %s", roomID, access.Mode)
	if access.CodePeriodSeconds > 0 {
		fmt.Printf(" (%s)", time.Duration(access.CodePeriodSeconds)*time.Second)
	}
	fmt.Println()
	return nil
}

func cmdRoomCode(ctx context.Context, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	code, err := client.RESTGetRoomAccessCode(ctx, *server, token, roomID)
	if err != nil {
		return err
	}
	fmt.Printf("%s (expires %s)\n", code.Code, time.UnixMilli(code.ExpiresAt).Format(time.RFC3339))
	return nil
}

func cmdRoomDelete(ctx context.Context, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTDeleteRoom(ctx, *server, token, roomID); err != nil {
		return err
	}
	fmt.Println("ok")
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
