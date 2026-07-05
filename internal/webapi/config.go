package webapi

// This file owns environment-backed web API configuration and the role-token
// normalization rules shared by server startup and tests.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains the HTTP listen settings, backend addresses, auth tokens,
// static asset path, CORS mode, and per-request backend timeout for webapi.
type Config struct {
	// Addr is the HTTP listen address for the web API process.
	Addr string
	// ControlAddr is the control-plane gRPC target used by HTTP handlers.
	ControlAddr string
	// LogAddr is the log-service gRPC target used by log explorer, audit, and registry reads.
	LogAddr string
	// APIToken authenticates backend gRPC calls and also acts as the legacy admin token.
	APIToken string
	// RoleTokens maps viewer/operator/admin web tokens to frontend permissions.
	RoleTokens map[role]string
	// StaticDir points to built frontend assets served by handleStatic.
	StaticDir string
	// DevCORS enables permissive local-development CORS handling.
	DevCORS bool
	// AllowUnauthenticated grants admin-equivalent access for local development only.
	AllowUnauthenticated bool
	// RequestTimeout bounds individual backend gRPC calls made while handling HTTP requests.
	RequestTimeout time.Duration
}

// DefaultConfig reads LOGSERVE_WEB_* and backend address environment variables
// and applies local-development defaults.
func DefaultConfig() Config {
	return Config{
		Addr:        getenv("LOGSERVE_WEB_ADDR", "127.0.0.1:8080"),
		ControlAddr: getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"),
		LogAddr:     getenv("LOGSERVE_LOGD_ADDR", "127.0.0.1:50051"),
		APIToken:    os.Getenv("LOGSERVE_API_TOKEN"),
		RoleTokens: map[role]string{
			roleViewer:   os.Getenv("LOGSERVE_VIEWER_TOKEN"),
			roleOperator: os.Getenv("LOGSERVE_OPERATOR_TOKEN"),
			roleAdmin:    os.Getenv("LOGSERVE_ADMIN_TOKEN"),
		},
		StaticDir:            getenv("LOGSERVE_WEB_STATIC_DIR", "web/dist"),
		DevCORS:              getenvBool("LOGSERVE_WEB_DEV_CORS", false),
		AllowUnauthenticated: getenvBool("LOGSERVE_WEB_ALLOW_UNAUTHENTICATED", false),
		RequestTimeout:       time.Duration(getenvInt("LOGSERVE_WEB_REQUEST_TIMEOUT_MS", 5000)) * time.Millisecond,
	}
}

// normalizeAuthConfig trims role tokens and keeps the historical APIToken/admin
// token aliases synchronized. Viewer/operator role tokens remain web-only.
func normalizeAuthConfig(cfg *Config) {
	cfg.APIToken = strings.TrimSpace(cfg.APIToken)
	if cfg.RoleTokens == nil {
		cfg.RoleTokens = make(map[role]string)
	}
	for _, roleName := range []role{roleViewer, roleOperator, roleAdmin} {
		cfg.RoleTokens[roleName] = strings.TrimSpace(cfg.RoleTokens[roleName])
	}
	// Keep the newer admin role token and the older backend APIToken alias aligned
	// so existing single-token deployments continue to authenticate both layers.
	if cfg.APIToken == "" && cfg.RoleTokens[roleAdmin] != "" {
		cfg.APIToken = cfg.RoleTokens[roleAdmin]
	}
	if cfg.APIToken != "" && cfg.RoleTokens[roleAdmin] == "" {
		cfg.RoleTokens[roleAdmin] = cfg.APIToken
	}
}

// hasConfiguredToken reports whether backend/API authentication has at least one
// admin-capable token configured.
func hasConfiguredToken(cfg Config) bool {
	return strings.TrimSpace(cfg.APIToken) != ""
}

// validateAuthConfig rejects duplicate role tokens so one presented bearer value
// cannot ambiguously map to multiple roles.
func validateAuthConfig(cfg Config) error {
	// Detect duplicates after trimming so a token never grants a role that depends
	// on iteration order through the candidate list.
	seen := make(map[string]role)
	for _, roleName := range []role{roleViewer, roleOperator, roleAdmin} {
		token := strings.TrimSpace(cfg.RoleTokens[roleName])
		if token == "" {
			continue
		}
		if existing, ok := seen[token]; ok && existing != roleName {
			return fmt.Errorf("%w: duplicate token configured for %s and %s", errInvalidInput, existing, roleName)
		}
		seen[token] = roleName
	}
	return nil
}

// getenv returns a trimmed environment value or the supplied fallback.
func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// getenvBool parses common boolean environment strings and falls back on empty
// or unrecognized values.
func getenvBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

// getenvInt parses an integer environment value and falls back on empty or
// malformed input.
func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
