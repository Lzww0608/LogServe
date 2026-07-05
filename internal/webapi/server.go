// Package webapi exposes LogServe control and log services as the HTTP API used
// by the web console. It owns route registration, request auth, JSON DTOs, SSE
// polling, and thin gRPC forwarding to the backend services.
package webapi

// This file wires the HTTP server, route table, static frontend fallback, and
// dashboard snapshot helper.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/logserve/logserve/gen/logservepb"
)

// Server owns the HTTP mux, normalized configuration, backend clients, and small
// in-memory caches used by web-console handlers.
type Server struct {
	// cfg is normalized during construction and then treated as read-mostly.
	cfg Config
	// clients owns the control/log gRPC clients shared by all handlers.
	clients *Clients
	// mux contains only raw route handlers; Handler wraps it with middleware.
	mux *http.ServeMux
	// functionRegistryCache tails system:functions across list/detail requests.
	functionRegistryCache *functionRegistryCache
}

// NewServer validates authentication settings, dials backend gRPC clients, and
// registers the HTTP routes. It returns an error before dialing when no usable
// token is configured and unauthenticated mode is disabled.
func NewServer(cfg Config) (*Server, error) {
	normalizeAuthConfig(&cfg)
	if !hasConfiguredToken(cfg) && !cfg.AllowUnauthenticated {
		return nil, fmt.Errorf("%w: LOGSERVE_API_TOKEN or LOGSERVE_ADMIN_TOKEN is required unless --allow-unauthenticated is set", errInvalidInput)
	}
	if err := validateAuthConfig(cfg); err != nil {
		return nil, err
	}
	clients, err := DialClients(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:                   cfg,
		clients:               clients,
		mux:                   http.NewServeMux(),
		functionRegistryCache: newFunctionRegistryCache(),
	}
	s.registerRoutes()
	return s, nil
}

// Handler returns the root HTTP handler wrapped with CORS, request-id, and auth
// middleware.
func (s *Server) Handler() http.Handler {
	return s.withMiddleware(s.mux)
}

// Close releases backend gRPC connections held by the server.
func (s *Server) Close() error {
	return s.clients.Close()
}

// registerRoutes maps console API endpoints to handlers, role gates, and audit
// action names. The role/action table is the authoritative backend permission
// map for web-console requests.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/healthz", s.handleHealthz)
	s.handleRoute("GET /api/session", roleViewer, "read_session", s.handleSession)
	s.handleRoute("GET /api/dashboard", roleViewer, "read_dashboard", s.handleDashboard)
	s.handleRoute("GET /api/events", roleViewer, "stream_events", s.handleEvents)
	s.handleRoute("GET /api/tasks", roleViewer, "list_tasks", s.handleListTasks)
	s.handleRoute("POST /api/tasks", roleOperator, "submit_task", s.handleSubmitTask)
	s.handleRoute("GET /api/tasks/{task_id}", roleViewer, "read_task", s.handleGetTask)
	s.handleRoute("POST /api/tasks/{task_id}/retry", roleOperator, "retry_task", s.handleRetryTask)
	s.handleRoute("POST /api/tasks/{task_id}/cancel", roleAdmin, "cancel_task", s.handleCancelTask)
	s.handleRoute("POST /api/tasks/{task_id}/resubmit", roleOperator, "resubmit_task", s.handleResubmitTask)
	s.handleRoute("GET /api/workflows", roleViewer, "list_workflows", s.handleListWorkflows)
	s.handleRoute("POST /api/workflows", roleOperator, "submit_workflow", s.handleSubmitWorkflow)
	s.handleRoute("POST /api/workflows/validate", roleOperator, "validate_workflow", s.handleValidateWorkflow)
	s.handleRoute("GET /api/workflows/{workflow_id}", roleViewer, "read_workflow", s.handleGetWorkflow)
	s.handleRoute("POST /api/workflows/{workflow_id}/replay", roleOperator, "replay_workflow", s.handleReplayWorkflow)
	s.handleRoute("GET /api/actors", roleOperator, "list_actors", s.handleListActors)
	s.handleRoute("POST /api/actors", roleOperator, "create_actor", s.handleCreateActor)
	s.handleRoute("GET /api/actors/{actor_id}", roleOperator, "read_actor", s.handleGetActor)
	s.handleRoute("POST /api/actors/{actor_id}/calls", roleOperator, "call_actor", s.handleCallActor)
	s.handleRoute("POST /api/actors/{actor_id}/replay", roleOperator, "replay_actor", s.handleReplayActor)
	s.handleRoute("GET /api/models", roleOperator, "list_models", s.handleListModels)
	s.handleRoute("POST /api/models", roleAdmin, "register_model", s.handleRegisterModel)
	s.handleRoute("POST /api/llm", roleOperator, "submit_llm", s.handleSubmitLLM)
	s.handleRoute("POST /api/llm/{task_id}/replay", roleOperator, "replay_llm", s.handleReplayLLM)
	s.handleRoute("GET /api/workers", roleOperator, "list_workers", s.handleListWorkers)
	s.handleRoute("GET /api/functions", roleOperator, "list_functions", s.handleListFunctions)
	s.handleRoute("GET /api/functions/{function_hash}", roleOperator, "read_function", s.handleGetFunction)
	s.handleRoute("GET /api/logs/streams", roleViewer, "list_log_streams", s.handleListLogStreams)
	s.handleRoute("GET /api/logs/streams/{stream_id}", roleViewer, "read_log_stream", s.handleReadLogStream)
	s.handleRoute("GET /api/logs/stats", roleViewer, "read_log_stats", s.handleLogStats)
	s.handleRoute("GET /api/templates", roleViewer, "list_templates", s.handleListTemplates)
	s.handleRoute("GET /api/templates/{template_id}", roleViewer, "read_template", s.handleGetTemplate)
	s.handleRoute("POST /api/templates/{template_id}/run", roleOperator, "run_template", s.handleRunTemplate)
	s.handleRoute("POST /api/admin/scheduling-policy", roleOperator, "set_scheduling_policy", s.handleSetSchedulingPolicy)
	s.handleRoute("POST /api/admin/backpressure", roleAdmin, "set_backpressure", s.handleSetBackpressure)
	s.handleRoute("GET /api/admin/config", roleAdmin, "read_admin_config", s.handleAdminConfig)
	s.mux.HandleFunc("/", s.handleStatic)
}

// handleHealthz returns process liveness and configured backend addresses; it is
// intentionally registered outside authenticated route handling.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"status":       "ok",
		"control_addr": s.cfg.ControlAddr,
		"log_addr":     s.cfg.LogAddr,
	})
}

// handleStatic serves built web assets and falls back to index.html for SPA deep
// links. API-looking paths are kept inside the JSON error contract.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "API endpoint not found")
		return
	}
	staticDir := strings.TrimSpace(s.cfg.StaticDir)
	if staticDir == "" {
		staticDir = "web/dist"
	}
	// Clean the URL path before joining so SPA deep links and odd slashes resolve
	// consistently under StaticDir. filepath.Join below still anchors relative paths
	// under StaticDir; API paths were rejected above before this static fallback.
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

// dashboard fetches one control-plane snapshot and converts it to the stable web
// DTO used by dashboard, list, and event handlers.
func (s *Server) dashboard(r *http.Request) (DashboardDTO, error) {
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.GetDashboardSnapshot(ctx, &logservepb.GetDashboardSnapshotRequest{})
	if err != nil {
		return DashboardDTO{}, err
	}
	return dashboardDTO(resp), nil
}
