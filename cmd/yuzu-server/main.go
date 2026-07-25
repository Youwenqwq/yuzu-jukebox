package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/youwenqwq/yuzu-jukebox/internal/app"
	"github.com/youwenqwq/yuzu-jukebox/internal/config"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults", *configPath)
		} else {
			log.Fatalf("load config: %v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg)
	if err != nil {
		log.Fatalf("init app: %v", err)
	}
	defer a.Store.Close()

	srv := &http.Server{Addr: cfg.Addr, Handler: a.Handler}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	log.Printf("yuzu-jukebox listening on %s", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}
