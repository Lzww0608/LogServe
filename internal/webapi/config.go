package webapi

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr                 string
	ControlAddr          string
	LogAddr              string
	APIToken             string
	RoleTokens           map[role]string
	StaticDir            string
	DevCORS              bool
	AllowUnauthenticated bool
	RequestTimeout       time.Duration
}

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

func normalizeAuthConfig(cfg *Config) {
	cfg.APIToken = strings.TrimSpace(cfg.APIToken)
	if cfg.RoleTokens == nil {
		cfg.RoleTokens = make(map[role]string)
	}
	for _, roleName := range []role{roleViewer, roleOperator, roleAdmin} {
		cfg.RoleTokens[roleName] = strings.TrimSpace(cfg.RoleTokens[roleName])
	}
	if cfg.APIToken == "" && cfg.RoleTokens[roleAdmin] != "" {
		cfg.APIToken = cfg.RoleTokens[roleAdmin]
	}
	if cfg.APIToken != "" && cfg.RoleTokens[roleAdmin] == "" {
		cfg.RoleTokens[roleAdmin] = cfg.APIToken
	}
}

func hasConfiguredToken(cfg Config) bool {
	return strings.TrimSpace(cfg.APIToken) != ""
}

func validateAuthConfig(cfg Config) error {
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
func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

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
