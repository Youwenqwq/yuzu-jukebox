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
		fmt.Println("（无 Player）")
		return nil
	}
	for _, player := range players {
		printPlayer(player)
	}
	return nil
}

func cmdPlayerShow(ctx context.Context, playerID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	player, err := client.RESTGetPlayer(ctx, *server, token, playerID)
	if err != nil {
		return err
	}
	printPlayer(player)
	return nil
}

func cmdPlayerCreate(ctx context.Context, playerID, name string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	credential, err := client.RESTCreatePlayer(ctx, *server, token, playerID, name)
	if err != nil {
		return err
	}
	fmt.Printf("player: %s (%s)\nkey: %s\n", credential.Player.ID, credential.Player.Name, credential.Key)
	fmt.Println("请立即保存 key；服务端不会再次显示。")
	return nil
}

func cmdPlayerRename(ctx context.Context, playerID, name string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	player, err := client.RESTUpdatePlayer(ctx, *server, token, playerID, client.PlayerUpdate{Name: &name})
	if err != nil {
		return err
	}
	printPlayer(player)
	return nil
}

func cmdPlayerSetActive(ctx context.Context, playerID string, active bool) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	player, err := client.RESTUpdatePlayer(ctx, *server, token, playerID, client.PlayerUpdate{Active: &active})
	if err != nil {
		return err
	}
	printPlayer(player)
	return nil
}

func cmdPlayerRotateKey(ctx context.Context, playerID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	credential, err := client.RESTRotatePlayerKey(ctx, *server, token, playerID)
	if err != nil {
		return err
	}
	fmt.Printf("key: %s\n", credential.Key)
	fmt.Println("请立即保存 key；旧 key 已失效，服务端不会再次显示。")
	return nil
}

func cmdPlayerDelete(ctx context.Context, playerID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTDeletePlayer(ctx, *server, token, playerID); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func printPlayer(player client.PlayerInfo) {
	status := "offline"
	if player.Online {
		status = "online"
	}
	active := "disabled"
	if player.Active {
		active = "active"
	}
	roomID := player.RoomID
	if roomID == "" {
		roomID = "-"
	}
	fmt.Printf("%s  %-20s %-8s %-8s room:%-12s", player.ID, player.Name, active, status, roomID)
	if player.Online {
		muted := ""
		if player.Muted {
			muted = " muted"
		}
		fmt.Printf(" volume:%3d%%%s device:%s caps:%s", player.Volume, muted, player.Device, strings.Join(player.Caps, ","))
	}
	fmt.Println()
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

func cmdPlayerUnbind(ctx context.Context, playerID, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	if err := client.RESTUnbindRoomPlayer(ctx, *server, token, roomID, playerID); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdRoomPlayers(ctx context.Context, roomID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	players, err := client.RESTRoomPlayers(ctx, *server, token, roomID)
	if err != nil {
		return err
	}
	if len(players) == 0 {
		fmt.Println("（无 headless player）")
		return nil
	}
	for _, player := range players {
		status := "offline"
		if player.Online {
			status = fmt.Sprintf("online volume:%d%%", player.Volume)
		}
		active := "disabled"
		if player.Active {
			active = "active"
		}
		fmt.Printf("%s  %-20s %-8s %-20s %s\n",
			player.ID, player.Name, active, status, player.Device)
	}
	return nil
}

func cmdRoomVolume(ctx context.Context, roomID, volumeText string) error {
	volume, err := strconv.Atoi(volumeText)
	if err != nil || volume < 0 || volume > 100 {
		return fmt.Errorf("音量须为 0-100 的整数")
	}
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	result, err := client.RESTRoomOutputSetVolume(ctx, *server, token, roomID, volume)
	if err != nil {
		return err
	}
	fmt.Printf("ok（已向 %d 个在线 Agent 下发）\n", result.Delivery.CommandsSent)
	return nil
}
