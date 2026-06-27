package webapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/logserve/logserve/gen/logservepb"
)

type Server struct {
	cfg     Config
	clients *Clients
	mux     *http.ServeMux
}

func NewServer(cfg Config) (*Server, error) {
	cfg.APIToken = strings.TrimSpace(cfg.APIToken)
	if cfg.APIToken == "" && !cfg.AllowUnauthenticated {
		return nil, fmt.Errorf("%w: LOGSERVE_API_TOKEN is required unless --allow-unauthenticated is set", errInvalidInput)
	}
	clients, err := DialClients(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:     cfg,
		clients: clients,
		mux:     http.NewServeMux(),
	}
	s.registerRoutes()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return s.withMiddleware(s.mux)
}

func (s *Server) Close() error {
	return s.clients.Close()
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	s.mux.HandleFunc("POST /api/tasks", s.handleSubmitTask)
	s.mux.HandleFunc("GET /api/tasks/{task_id}", s.handleGetTask)
	s.mux.HandleFunc("GET /api/workflows", s.handleListWorkflows)
	s.mux.HandleFunc("POST /api/workflows", s.handleSubmitWorkflow)
	s.mux.HandleFunc("POST /api/workflows/validate", s.handleValidateWorkflow)
	s.mux.HandleFunc("GET /api/workflows/{workflow_id}", s.handleGetWorkflow)
	s.mux.HandleFunc("POST /api/workflows/{workflow_id}/replay", s.handleReplayWorkflow)
	s.mux.HandleFunc("GET /api/actors", s.handleListActors)
	s.mux.HandleFunc("POST /api/actors", s.handleCreateActor)
	s.mux.HandleFunc("GET /api/actors/{actor_id}", s.handleGetActor)
	s.mux.HandleFunc("POST /api/actors/{actor_id}/calls", s.handleCallActor)
	s.mux.HandleFunc("POST /api/actors/{actor_id}/replay", s.handleReplayActor)
	s.mux.HandleFunc("GET /api/models", s.handleListModels)
	s.mux.HandleFunc("POST /api/models", s.handleRegisterModel)
	s.mux.HandleFunc("POST /api/llm", s.handleSubmitLLM)
	s.mux.HandleFunc("POST /api/llm/{task_id}/replay", s.handleReplayLLM)
	s.mux.HandleFunc("GET /api/workers", s.handleListWorkers)
	s.mux.HandleFunc("POST /api/admin/scheduling-policy", s.handleSetSchedulingPolicy)
	s.mux.HandleFunc("GET /api/admin/config", s.handleAdminConfig)
	s.mux.HandleFunc("/", s.handleStatic)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"status":       "ok",
		"control_addr": s.cfg.ControlAddr,
		"log_addr":     s.cfg.LogAddr,
	})
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "API endpoint not found")
		return
	}
	staticDir := strings.TrimSpace(s.cfg.StaticDir)
	if staticDir == "" {
		staticDir = "web/dist"
	}
	cleanPath := strings.TrimPrefix(filepath.Clean(r.URL.Path), string(os.PathSeparator))
	path := filepath.Join(staticDir, cleanPath)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		http.ServeFile(w, r, path)
		return
	}
	index := filepath.Join(staticDir, "index.html")
	if _, err := os.Stat(index); err == nil {
		http.ServeFile(w, r, index)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<html><body><h1>LogServe Console</h1><p>Static frontend not found. Build web assets or run the Vite dev server.</p></body></html>`))
}

func (s *Server) dashboard(r *http.Request) (DashboardDTO, error) {
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.GetDashboardSnapshot(ctx, &logservepb.GetDashboardSnapshotRequest{})
	if err != nil {
		return DashboardDTO{}, err
	}
	return dashboardDTO(resp), nil
}
