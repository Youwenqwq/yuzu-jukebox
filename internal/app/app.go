// Package app 组装全部依赖，产出可运行的 http.Handler。
// main 与集成测试共用这一入口。
package app

import (
	"context"
	"net/http"
	"os"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/config"
	"github.com/youwenqwq/yuzu-jukebox/internal/httpapi"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider/bili"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider/local"
	"github.com/youwenqwq/yuzu-jukebox/internal/provider/ncm"
	"github.com/youwenqwq/yuzu-jukebox/internal/room"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
	"github.com/youwenqwq/yuzu-jukebox/internal/wsapi"
)

type App struct {
	Handler http.Handler
	Store   *store.Store
}

func New(ctx context.Context, cfg config.Config) (*App, error) {
	for _, dir := range []string{cfg.MediaDir, cfg.CacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	authm := auth.NewManager(cfg.AdminPassword)
	reg := provider.NewRegistry()
	lp := local.New(cfg.MediaDir, st)
	reg.Register(lp)
	if cfg.NCM.Enabled {
		reg.Register(ncm.New(cfg.NCM.BaseURL, cfg.NCM.Level, st))
	}
	if cfg.Bili.Enabled {
		reg.Register(bili.New(cfg.Bili.BaseURL, st))
	}

	c := cache.New(cfg.CacheDir, cfg.CacheMaxBytes, st, reg)

	rooms := room.NewManager(ctx, st, authm, c, reg)
	if err := rooms.Load(); err != nil {
		return nil, err
	}

	ws := wsapi.NewServer(authm, rooms, reg)
	api := httpapi.NewServer(st, authm, rooms, reg, lp, c, ws)

	return &App{Handler: api.Handler(), Store: st}, nil
}
