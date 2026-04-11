package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lenisko/rampardos/internal/services"
)

// DebugRequestBody logs POST/PUT/PATCH request bodies when debug mode is enabled at runtime
func DebugRequestBody() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check runtime debug setting
			if services.GlobalRuntimeSettings == nil || !services.GlobalRuntimeSettings.IsDebugEnabled() {
				next.ServeHTTP(w, r)
				return
			}

			if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
				contentType := r.Header.Get("Content-Type")

				// Read body
				body, err := io.ReadAll(r.Body)
				if err != nil {
					fmt.Printf("[DEBUG] Failed to read request body: %v\n", err)
					next.ServeHTTP(w, r)
					return
				}
				// Restore body for handler
				r.Body = io.NopCloser(bytes.NewBuffer(body))

				// Log based on content type (use fmt.Printf to avoid escaping)
				if strings.Contains(contentType, "multipart/form-data") {
					fmt.Printf("[DEBUG] %s %s Content-Type: %s Content-Length: %d (multipart body not logged)\n",
						r.Method, r.URL.Path, contentType, len(body))
				} else {
					fmt.Printf("[DEBUG] %s %s\n%s\n", r.Method, r.URL.Path, string(body))
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}
