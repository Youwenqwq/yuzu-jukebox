package httpapi

import (
	"net/http"
	"strings"

	"github.com/youwenqwq/yuzu-jukebox/internal/config"
)

// corsMiddleware 包装 http.Handler 注入 CORS 响应头。
// 预检请求（OPTIONS）直接 204 返回，不向后传递。
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

	allowedMethods := strings.Join(cfg.AllowedMethods, ", ")
	allowedHeaders := strings.Join(cfg.AllowedHeaders, ", ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// 无 Origin 头（非浏览器同源请求）不处理。
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		// 校验 Origin。
		if !allowAllOrigins && !originMap[origin] {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		if cfg.AllowCredentials {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		// 预检请求。
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", allowedMethods)
			w.Header().Set("Access-Control-Allow-Headers", allowedHeaders)
			if cfg.MaxAge > 0 {
				w.Header().Set("Access-Control-Max-Age", itoa(cfg.MaxAge))
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// 预检以外：CORS 头放在 Vary，指示缓存需按 Origin 区分。
		w.Header().Add("Vary", "Origin")
		next.ServeHTTP(w, r)
	})
}

// itoa 是 strconv.Itoa 免导入的内联版本。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 12)
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
