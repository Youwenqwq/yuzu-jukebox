package httpapi

import (
	"net/http"

	"github.com/youwenqwq/yuzu-jukebox/internal/config"
)

// CORSMiddleware 包装 http.Handler 注入 CORS 响应头。
// 方法/头/有效期硬编码为宽泛的安全默认值，用户只需配置 origin。
func CORSMiddleware(cfg config.CORSConfig, next http.Handler) http.Handler {
	if !cfg.Enabled {
		return next
	}

	allowAllOrigins := false
	originMap := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			allowAllOrigins = true
		}
		originMap[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		if !allowAllOrigins && !originMap[origin] {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		if cfg.AllowCredentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Token, Range")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.Header().Add("Vary", "Origin")
		next.ServeHTTP(w, r)
	})
}
