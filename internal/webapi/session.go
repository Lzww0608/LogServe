package webapi

// This file exposes the current authenticated principal and role-derived
// frontend permission list.

import "net/http"

// handleSession returns the authenticated principal and frontend permission list
// for the current role token.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	principal := principalFromRequest(r)
	writeJSON(w, map[string]any{
		"subject":     principal.Subject,
		"role":        principal.Role,
		"permissions": permissionsForRole(principal.Role),
	})
}

// permissionsForRole maps backend roles to the feature flags consumed by the web
// console UI.
func permissionsForRole(value role) []string {
	permissions := []string{"read:dashboard", "read:tasks", "read:workflows", "read:logs", "read:templates"}
	if roleAllows(value, roleOperator) {
		permissions = append(permissions,
			"submit:tasks",
			"submit:workflows",
			"call:actors",
			"replay",
			"set:scheduling",
			"run:templates",
		)
	}
	if roleAllows(value, roleAdmin) {
		permissions = append(permissions,
			"set:backpressure",
			"register:models",
			"cancel:tasks",
			"dangerous:actions",
		)
	}
	return permissions
}
