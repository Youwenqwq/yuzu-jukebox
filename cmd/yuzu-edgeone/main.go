// yuzu-edgeone publishes complete cached tracks to EdgeOne Blob without
// adding EdgeOne credentials or SDKs to yuzu-server.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/youwenqwq/yuzu-jukebox/internal/edgeonepublisher"
)

func main() {
	configPath := flag.String("config", "edgeone.json", "path to yuzu-edgeone config")
	once := flag.Bool("once", false, "publish at most one pending object, then exit")
	flag.Parse()

	cfg, err := edgeonepublisher.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	state, err := edgeonepublisher.OpenState(cfg.StatePath)
	if err != nil {
		log.Fatal(err)
	}
	defer state.Close()

	publisher := edgeonepublisher.New(cfg, state)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *once {
		if err := publisher.PublishOnce(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := publisher.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
