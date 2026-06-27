package webapi

import "net/http"

func (s *Server) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"workers": dashboard.Workers})
}
