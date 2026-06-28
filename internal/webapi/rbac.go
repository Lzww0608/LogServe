package webapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

type role string

const (
	roleViewer   role = "viewer"
	roleOperator role = "operator"
	roleAdmin    role = "admin"
)

type authPrincipal struct {
	Subject string `json:"subject"`
	Role    role   `json:"role"`
}

type authContextKey struct{}

func withPrincipal(ctx context.Context, principal authPrincipal) context.Context {
	return context.WithValue(ctx, authContextKey{}, principal)
}

func principalFromRequest(r *http.Request) authPrincipal {
	if principal, ok := r.Context().Value(authContextKey{}).(authPrincipal); ok {
		return principal
	}
	return authPrincipal{Subject: "unknown", Role: ""}
}

func authenticateHTTP(header string, cfg Config) (authPrincipal, bool) {
	if cfg.AllowUnauthenticated {
		return authPrincipal{Subject: "anonymous", Role: roleAdmin}, true
	}
	presented := bearerToken(header)
	if presented == "" {
		return authPrincipal{}, false
	}
	for _, candidate := range []struct {
		role  role
		token string
	}{
		{role: roleAdmin, token: cfg.RoleTokens[roleAdmin]},
		{role: roleOperator, token: cfg.RoleTokens[roleOperator]},
		{role: roleViewer, token: cfg.RoleTokens[roleViewer]},
		{role: roleAdmin, token: cfg.APIToken},
	} {
		if tokenMatches(presented, candidate.token) {
			return authPrincipal{Subject: string(candidate.role) + ":" + tokenFingerprint(presented), Role: candidate.role}, true
		}
	}
	return authPrincipal{}, false
}

func bearerToken(header string) string {
	presented := strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(presented), "bearer ") {
		presented = strings.TrimSpace(presented[len("bearer "):])
	}
	return presented
}

func tokenMatches(presented, configured string) bool {
	configured = strings.TrimSpace(configured)
	if configured == "" || len(presented) != len(configured) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
}

func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

func (s *Server) handleRoute(pattern string, minimum role, action string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromRequest(r)
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		responseWriter := http.ResponseWriter(recorder)
		if _, ok := w.(http.Flusher); ok {
			responseWriter = &flushingStatusRecorder{statusRecorder: recorder}
		}
		if !roleAllows(principal.Role, minimum) {
			writeAPIError(responseWriter, http.StatusForbidden, "PERMISSION_DENIED", "insufficient role for this operation")
		} else {
			handler(responseWriter, r)
		}
		if action != "" {
			statusCode := recorder.status
			if statusCode == 0 {
				statusCode = http.StatusOK
			}
			s.auditFrontendOperation(r, principal, action, statusCode, started)
		}
	})
}

func roleAllows(actual, required role) bool {
	return roleRank(actual) >= roleRank(required)
}

func roleRank(value role) int {
	switch value {
	case roleAdmin:
		return 3
	case roleOperator:
		return 2
	case roleViewer:
		return 1
	default:
		return 0
	}
}
