package webapi

// This file applies CORS, request-id propagation, and HTTP bearer
// authentication before requests reach route handlers.

import (
	"net/http"
	"strings"
)

// withMiddleware applies development CORS, injects request IDs for API routes,
// and authenticates every API path except healthz before routing.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevCORS {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			var id string
			r, id = ensureRequestID(r)
			w.Header().Set("X-Request-ID", id)
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/healthz" {
			principal, ok := authenticateHTTP(r.Header.Get("Authorization"), s.cfg)
			if !ok {
				writeAPIError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "missing or invalid API token")
				return
			}
			r = r.WithContext(withPrincipal(r.Context(), principal))
		}
		next.ServeHTTP(w, r)
	})
}

// authorizedHTTP preserves the simple bearer-token check used by older tests and
// helpers.
func authorizedHTTP(header, token string) bool {
	return tokenMatches(bearerToken(header), token)
}
