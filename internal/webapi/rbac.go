package webapi

// This file defines console roles, bearer-token authentication, and the route
// wrapper that combines RBAC with audit recording.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// role is the ordered permission level recognized by the web console API.
type role string

// Console roles are ordered from read-only to full administrative access.
const (
	// roleViewer can read dashboard, logs, templates, and session state.
	roleViewer role = "viewer"
	// roleOperator can submit and replay runtime work but cannot change admin-only settings.
	roleOperator role = "operator"
	// roleAdmin can perform privileged operations such as model registration and backpressure changes.
	roleAdmin role = "admin"
)

// authPrincipal is the authenticated subject stored in request context and audit
// records.
type authPrincipal struct {
	// Subject is a non-secret audit identifier derived from the accepted token.
	Subject string `json:"subject"`
	// Role is the permission level granted to the request.
	Role role `json:"role"`
}

// authContextKey avoids collisions for authPrincipal values stored in contexts.
type authContextKey struct{}

// withPrincipal attaches the authenticated principal to a request context.
func withPrincipal(ctx context.Context, principal authPrincipal) context.Context {
	return context.WithValue(ctx, authContextKey{}, principal)
}

// principalFromRequest returns the authenticated principal or an unknown
// placeholder when a handler is reached without auth context.
func principalFromRequest(r *http.Request) authPrincipal {
	if principal, ok := r.Context().Value(authContextKey{}).(authPrincipal); ok {
		return principal
	}
	return authPrincipal{Subject: "unknown", Role: ""}
}

// authenticateHTTP maps a presented bearer token to a role. AllowUnauthenticated
// deliberately grants admin for local development; otherwise admin, operator,
// viewer, and legacy APIToken values are checked in privilege order.
func authenticateHTTP(header string, cfg Config) (authPrincipal, bool) {
	if cfg.AllowUnauthenticated {
		return authPrincipal{Subject: "anonymous", Role: roleAdmin}, true
	}
	presented := bearerToken(header)
	if presented == "" {
		return authPrincipal{}, false
	}
	// Check higher privileges first so the admin/APIToken alias cannot be
	// shadowed by a lower-role token if a config mistake slips past validation.
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

// bearerToken accepts both raw tokens and Authorization: Bearer values.
func bearerToken(header string) string {
	// Accepting a raw token keeps older tests and simple CLI probes working while
	// normal browser calls still use the standard Bearer form.
	presented := strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(presented), "bearer ") {
		presented = strings.TrimSpace(presented[len("bearer "):])
	}
	return presented
}

// tokenMatches compares equal-length tokens in constant time and rejects empty
// configured tokens.
func tokenMatches(presented, configured string) bool {
	configured = strings.TrimSpace(configured)
	if configured == "" || len(presented) != len(configured) {
		// Length mismatch is rejected before ConstantTimeCompare; this leaks only
		// token length and avoids treating an empty configured token as valid.
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
}

// tokenFingerprint returns a short non-secret token identifier for subjects and
// audit records.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

// handleRoute wraps a route with RBAC enforcement and audit emission. It records
// the final HTTP status while preserving http.Flusher for SSE handlers.
func (s *Server) handleRoute(pattern string, minimum role, action string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromRequest(r)
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		responseWriter := http.ResponseWriter(recorder)
		// Streaming routes need Flush to survive the response recorder wrapper.
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

// roleAllows compares roles by rank so higher roles inherit lower-role access.
func roleAllows(actual, required role) bool {
	return roleRank(actual) >= roleRank(required)
}

// roleRank returns the numeric ordering used for RBAC checks.
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
