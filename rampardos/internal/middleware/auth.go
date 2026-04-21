package middleware

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/lenisko/rampardos/internal/services"
)

// AdminAuth creates a middleware for HTTP Basic Authentication on admin routes
func AdminAuth() func(http.Handler) http.Handler {
	username := strings.TrimSpace(os.Getenv("ADMIN_USERNAME"))
	password := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD"))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// If no credentials configured, dashboard is disabled
			if username == "" || password == "" {
				services.GlobalMetrics.RecordHTTPError("admin_auth", http.StatusUnauthorized)
				http.Error(w, "Dashboard Disabled!", http.StatusUnauthorized)
				return
			}

			// Get Basic Auth credentials
			reqUser, reqPass, ok := r.BasicAuth()
			if !ok {
				services.GlobalMetrics.RecordHTTPError("admin_auth", http.StatusUnauthorized)
				w.Header().Set("WWW-Authenticate", `Basic realm="Admin"`)
				http.Error(w, "Login Required!", http.StatusUnauthorized)
				return
			}

			// Constant-time comparison to prevent timing attacks
			userMatch := subtle.ConstantTimeCompare([]byte(reqUser), []byte(username)) == 1
			passMatch := subtle.ConstantTimeCompare([]byte(reqPass), []byte(password)) == 1

			if !userMatch || !passMatch {
				services.GlobalMetrics.RecordHTTPError("admin_auth", http.StatusUnauthorized)
				w.Header().Set("WWW-Authenticate", `Basic realm="Admin"`)
				http.Error(w, "Invalid Login!", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
