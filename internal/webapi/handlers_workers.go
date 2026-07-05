package webapi

// This file exposes worker dashboard rows through the HTTP API.

import "net/http"

// handleListWorkers returns worker rows from the dashboard snapshot.
func (s *Server) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	// Worker state is dashboard-derived rather than queried per worker, keeping
	// the endpoint cheap and consistent with the SSE dashboard snapshot.
	dashboard, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"workers": dashboard.Workers})
}
