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
	"github.com/youwenqwq/yuzu-jukebox/internal/control"
	"github.com/youwenqwq/yuzu-jukebox/internal/credmon"
	"github.com/youwenqwq/yuzu-jukebox/internal/distribution"
	"github.com/youwenqwq/yuzu-jukebox/internal/httpapi"
	"github.com/youwenqwq/yuzu-jukebox/internal/plsync"
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
	Control *control.Service
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
	integrations := auth.NewIntegrationRegistry(st)
	bindings := auth.NewBindingService(st)
	playerAuth := auth.NewPlayerRegistry(st)

	authm := auth.NewManager(cfg.AdminPassword, st)
	go runSessionJanitor(ctx, authm, st)
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

	distributionService := distribution.New(st)
	accelerationRegistry := distribution.NewRegistry(st)
	healthMonitor := distribution.NewHealthMonitor(st)
	go healthMonitor.Run(ctx)
	go runAccelerationInventoryScheduler(ctx, st)
	go runAccelerationPinSweeper(ctx, distributionService)
	c.SetReadyHook(func(ref provider.TrackRef) {
		if err := distributionService.RequestCacheReady(context.Background(), ref); err != nil {
			log.Printf("[distribution] request %s failed: %v", ref, err)
		}
	})

	rooms := room.NewManager(ctx, st, authm, c, reg, key)
	if err := rooms.Load(); err != nil {
		return nil, err
	}

	controls := control.NewService(rooms, reg, control.NewAuthorizer(st))

	mon := credmon.New(reg, st)
	go mon.Run(ctx)
	if cfg.PlaylistSyncIntervalMinutes > 0 {
		syncer := plsync.New(reg, st)
		syncer.SetInterval(time.Duration(cfg.PlaylistSyncIntervalMinutes) * time.Minute)
		go syncer.Run(ctx)
	}

	var oidcValidator *auth.OIDCValidator
	if cfg.OIDC.Enabled && cfg.OIDC.Issuer != "" {
		oidcValidator = auth.NewOIDCValidator(cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.ExtraClientIDs...)
		log.Printf("oidc: issuer %s (client_id %s, extras %v)", cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.ExtraClientIDs)
	}

	ws := wsapi.NewServer(authm, playerAuth, controls, st)
	api := httpapi.NewServer(st, authm, integrations, bindings, rooms, reg, lp, c, controls, ws, oidcValidator, cfg.OIDC.RoleMapping, cfg.NCM.CoverDirect)
	api.SetCoverSecret(key)
	api.ConfigureDistribution(distributionService, accelerationRegistry)

	if cfg.CacheAutoPruneDays > 0 {
		go runCacheJanitor(ctx, c, cfg.CacheAutoPruneDays)
	}

	return &App{
		Handler: httpapi.CORSMiddleware(cfg.CORS, api.Handler()),
		Store:   st,
		Control: controls,
	}, nil
}

func runSessionJanitor(ctx context.Context, manager *auth.Manager, st *store.Store) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := manager.PruneExpired(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[auth] session prune failed: %v", err)
			}
			if err := st.PruneIdempotency(ctx, now.UnixMilli()); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[http] idempotency prune failed: %v", err)
			}
			if err := st.PruneExternalBindingCodes(ctx, now.UnixMilli()); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[auth] binding code prune failed: %v", err)
			}
		}
	}
}

func runAccelerationInventoryScheduler(ctx context.Context, st *store.Store) {
	schedule := func(now time.Time) {
		scans, err := st.ScheduleDueAccelerationInventoryScans(ctx, now.UnixMilli())
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("[distribution] schedule inventory scans: %v", err)
			}
			return
		}
		for _, scan := range scans {
			log.Printf("[distribution] scheduled inventory scan %s for %s", scan.ID, scan.AccelerationID)
		}
	}
	schedule(time.Now())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			schedule(now)
		}
	}
}

// runAccelerationPinSweeper 周期性地把房间预取视界登记为加速需求并钉住。
//
// 用轮询而不是让房间 actor 推送：钉住是 deadline 形状的，房间崩溃或通知丢失时会自行
// 过期，所以定期重刷是最省事也最不会泄漏的形态，而且分发层不必反向依赖房间层。
// 扫描周期必须显著短于 distribution.PinTTL，单次失败不能让正在播放的对象失去保护。
func runAccelerationPinSweeper(ctx context.Context, service *distribution.Service) {
	sweep := func() {
		if err := service.PinPrefetchHorizon(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[distribution] pin prefetch horizon: %v", err)
		}
	}
	sweep()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
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
