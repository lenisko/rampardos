package middleware

import (
	"log/slog"
	"net/http"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// RequestLogger is a slog-based access logger. It replaces chi's
// middleware.Logger so request logs share the structured slog format
// used by the rest of the server, and so proxy headers
// (X-Forwarded-For, X-Real-IP, CF-Connecting-IP) are captured —
// chi's RealIP only honours the first two, and only logs the result
// in r.RemoteAddr without surfacing the original chain.
func RequestLogger() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			attrs := []any{
				"method", r.Method,
				"path", r.URL.RequestURI(),
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration", time.Since(start),
				"remote", r.RemoteAddr,
			}
			if v := r.Header.Get("X-Forwarded-For"); v != "" {
				attrs = append(attrs, "x_forwarded_for", v)
			}
			if v := r.Header.Get("X-Real-IP"); v != "" {
				attrs = append(attrs, "x_real_ip", v)
			}
			if v := r.Header.Get("CF-Connecting-IP"); v != "" {
				attrs = append(attrs, "cf_connecting_ip", v)
			}
			if v := r.Header.Get("User-Agent"); v != "" {
				attrs = append(attrs, "user_agent", v)
			}
			slog.Info("request", attrs...)
		})
	}
}
