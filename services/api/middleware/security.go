package middleware

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

const (
	envCORSDevMode    = "CORS_DEV_MODE"
	envAllowedOrigins = "ALLOWED_ORIGINS"
)

// reValidOrigin validates that an origin is a reasonable https:// or http://localhost URL.
// This catches typos and basic misconfigurations at startup.
var reValidOrigin = regexp.MustCompile(`^https://[a-zA-Z0-9.-]+(?::\d{1,5})?$|^http://localhost(?::\d{1,5})?$`)

// ValidateAllowedOrigins parses and validates the ALLOWED_ORIGINS env var and the
// CORS_DEV_MODE flag. It is intended to be called at startup so misconfiguration
// causes a hard failure rather than a runtime CORS error.
//
// Rules:
//   - If ALLOWED_ORIGINS is empty or "*" and CORS_DEV_MODE is not "true", return an error.
//   - If ALLOWED_ORIGINS is "*" and CORS_DEV_MODE is "true", allow wildcard (dev mode).
//   - Otherwise each origin must be a valid https:// URL or http://localhost.
func ValidateAllowedOrigins() ([]string, error) {
	raw := os.Getenv(envAllowedOrigins)
	devMode := os.Getenv(envCORSDevMode) == "true"

	if raw == "" || raw == "*" {
		if !devMode {
			return nil, fmt.Errorf("ALLOWED_ORIGINS must be a comma-separated list of origins in production; set CORS_DEV_MODE=true to allow wildcard")
		}
		return nil, nil // wildcard
	}

	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		o := strings.TrimSpace(p)
		if o == "" {
			continue
		}
		if o == "*" && devMode {
			return nil, nil // wildcard in dev mode
		}
		if !reValidOrigin.MatchString(o) {
			return nil, fmt.Errorf("invalid origin %q in ALLOWED_ORIGINS: must be https://host[:port] or http://localhost[:port]", o)
		}
		origins = append(origins, o)
	}

	return origins, nil
}

// SecurityHeaders returns middleware that sets standard security headers on
// every response. HSTS is only set when TLS is enforced (production).
func SecurityHeaders(isProduction bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()

			// X-Content-Type-Options: prevent MIME-sniffing (all responses).
			h.Set("X-Content-Type-Options", "nosniff")

			// Referrer-Policy: no referrer for cross-origin requests.
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")

			// X-Frame-Options: deny — no iframing of API responses.
			h.Set("X-Frame-Options", "DENY")

			// X-XSS-Protection: deprecated but still respected by some older browsers.
			h.Set("X-XSS-Protection", "0")

			// HSTS (HTTP Strict Transport Security) — only in production.
			if isProduction {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
			}

			next.ServeHTTP(w, r)
		})
	}
}
