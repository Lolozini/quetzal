package api

import "net/http"

// contentSecurityPolicy is the panel's policy. The UI is a Vite build with no
// runtime dependency and no inline script or style, so 'self' is enough
// everywhere and nothing needs 'unsafe-inline'. The console WebSocket is
// same-origin, which 'self' covers in connect-src.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"object-src 'none'"

// SecurityHeaders sets the response headers a browser needs to be told, since it
// assumes the worst by default: don't sniff a type, don't frame this page, don't
// leak the URL on the way out. It wraps the whole panel — the API, the SPA and
// the ops endpoints — because the file manager serves tenant-controlled bytes
// from the same origin as the session cookie.
//
// A handler is free to override any of these before it writes (the docs viewer
// does, for the CDN it loads).
func (s *Server) SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		// Only when the panel is known to be behind TLS: asserting HSTS from an
		// http-only install would pin a browser to a scheme that does not answer.
		// Same signal as the Secure flag on the session cookie.
		if s.Secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}
