// Package app 组装全部依赖，产出可运行的 http.Handler。
// main 与集成测试共用这一入口。
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/auth"
	"github.com/youwenqwq/yuzu-jukebox/internal/cache"
	"github.com/youwenqwq/yuzu-jukebox/internal/config"
	"github.com/youwenqwq/yuzu-jukebox/internal/credmon"
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

	key, err := cfg.SecretKeyBytes()
	if err != nil {
		return nil, fmt.Errorf("secret_key: %w", err)
	}
	st, err := store.Open(cfg.DBPath, key)
	if err != nil {
		return nil, err
	}

	authm := auth.NewManager(cfg.AdminPassword, st)
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

	mon := credmon.New(reg, st)
	go mon.Run(ctx)

	var oidcValidator *auth.OIDCValidator
	if cfg.OIDC.Enabled && cfg.OIDC.Issuer != "" {
		oidcValidator = auth.NewOIDCValidator(cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.ExtraClientIDs...)
		log.Printf("oidc: issuer %s (client_id %s, extras %v)", cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.ExtraClientIDs)
	}

	ws := wsapi.NewServer(authm, rooms, reg)
	api := httpapi.NewServer(st, authm, rooms, reg, lp, c, ws, oidcValidator, cfg.OIDC.RoleMapping)

	if cfg.CacheAutoPruneDays > 0 {
		go runCacheJanitor(ctx, c, cfg.CacheAutoPruneDays)
	}

	return &App{Handler: httpapi.CORSMiddleware(cfg.CORS, api.Handler()), Store: st}, nil
}

func runCacheJanitor(ctx context.Context, c *cache.Cache, unusedDays int) {
	initial := time.NewTimer(time.Minute)
	defer initial.Stop()
	select {
	case <-ctx.Done():
		return
	case <-initial.C:
	}

	prune := func() {
		evicted, freed, err := c.PruneUnused(ctx, time.Duration(unusedDays)*24*time.Hour)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("[cache] auto prune failed: %v", err)
			}
			return
		}
		log.Printf("[cache] auto prune: evicted %d entries, freed %d bytes", evicted, freed)
	}
	prune()

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
