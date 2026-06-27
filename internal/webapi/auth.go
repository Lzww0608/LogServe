package webapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevCORS {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/healthz" && !s.cfg.AllowUnauthenticated {
			if !authorizedHTTP(r.Header.Get("Authorization"), s.cfg.APIToken) {
				writeAPIError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "missing or invalid API token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func authorizedHTTP(header, token string) bool {
	presented := strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(presented), "bearer ") {
		presented = strings.TrimSpace(presented[len("bearer "):])
	}
	if len(presented) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}
