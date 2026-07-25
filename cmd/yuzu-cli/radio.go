// radio.go — 电台模式命令实现。
package main

import (
	"context"
	"fmt"
)

func cmdRadioPlay(ctx context.Context, roomID, source string) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := cli.RadioPlay(ctx, roomID, source, *shuffle, *once); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdRadioStop(ctx context.Context, roomID string) error {
	cli, err := connect(ctx, roomID)
	if err != nil {
		return err
	}
	defer cli.Close()
	if err := cli.RadioStop(ctx, roomID); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}
