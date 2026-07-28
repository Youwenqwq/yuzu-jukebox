package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func cmdPlayers(ctx context.Context) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	players, err := client.RESTPlayers(ctx, *server, token)
	if err != nil {
		return err
	}
	if len(players) == 0 {
		fmt.Println("（无在线播放端）")
		return nil
	}
	for _, p := range players {
		room := p.RoomID
		if room == "" {
			room = "-"
		}
		mute := ""
		if p.Muted {
			mute = " [静音]"
		}
		fmt.Printf("%s  %-16s %-12s 房间:%-12s 音量:%3d%%%s  (%s)\n",
			p.ID, p.Device, p.Identity, room, p.Volume, mute,
			strings.Join(p.Caps, ","))
	}
	return nil
}

func cmdPlayerVolume(ctx context.Context, playerID string, volStr string) error {
	vol, err := strconv.Atoi(volStr)
	if err != nil || vol < 0 || vol > 100 {
		return fmt.Errorf("音量须为 0-100 的整数")
	}
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTPlayerCommand(ctx, *server, token, playerID, "set_volume", vol); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdPlayerMute(ctx context.Context, playerID, onoff string) error {
	var muted bool
	switch strings.ToLower(onoff) {
	case "on", "1", "true", "是":
		muted = true
	case "off", "0", "false", "否":
		muted = false
	default:
		return fmt.Errorf("用法: player mute <id> on|off")
	}
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTPlayerCommand(ctx, *server, token, playerID, "set_mute", muted); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdPlayerBind(ctx context.Context, playerID, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if _, err := client.RESTBindRoomPlayer(ctx, *server, token, roomID, playerID); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdPlayerUnbind(ctx context.Context, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTUnbindRoomPlayer(ctx, *server, token, roomID); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}
