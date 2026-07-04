package observability

import (
	"log"
	"net"
	"net/http"
	// Import net/http/pprof for its handler-registration side effect on http.DefaultServeMux.
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Default profile sampling rates keep mutex and block profiles useful when the
// environment overrides are unset, malformed, or non-positive.
const (
	defaultMutexProfileFraction = 10
	defaultBlockProfileRate     = 10000
)

// EnableRuntimeProfiles turns on mutex and block profiling samples used by
// /debug/pprof/mutex and /debug/pprof/block.
//
// This changes process-global runtime profiling knobs. Invalid, empty, zero,
// or negative environment overrides are ignored so a malformed deployment
// value does not silently disable the profiles requested by StartDebugServer.
func EnableRuntimeProfiles() {
	mutexFraction := defaultMutexProfileFraction
	if v := os.Getenv("LOGSERVE_MUTEX_PROFILE_FRACTION"); v != "" {
		// Only positive values are accepted; runtime treats zero as disabling
		// mutex profiling, which would be surprising after opting in.
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			mutexFraction = parsed
		}
	}
	blockRate := defaultBlockProfileRate
	if v := os.Getenv("LOGSERVE_BLOCK_PROFILE_RATE"); v != "" {
		// Keep the same positive-value rule as mutex profiling so both profile
		// controls fail closed to useful defaults.
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			blockRate = parsed
		}
	}
	runtime.SetMutexProfileFraction(mutexFraction)
	runtime.SetBlockProfileRate(blockRate)
}

// StartDebugServer serves runtime profiles on addr when non-empty.
// Typical endpoints: /debug/pprof/profile, /heap, /mutex, /block.
//
// The server runs in a background goroutine and has no shutdown hook because it
// is diagnostics-only process state. Passing an empty or whitespace-only addr is
// a no-op. The nil handler passed to http.Serve intentionally uses
// http.DefaultServeMux, where the net/http/pprof side-effect import registers
// the debug endpoints.
func StartDebugServer(addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	EnableRuntimeProfiles()
	go func() {
		// Bind inside the goroutine so a pprof port conflict is reported without
		// preventing the main service from continuing to start.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("pprof listen failed addr=%s err=%v", addr, err)
			return
		}
		log.Printf("pprof listening on http://%s/debug/pprof/", ln.Addr().String())
		if err := http.Serve(ln, nil); err != nil {
			log.Printf("pprof server stopped addr=%s err=%v", addr, err)
		}
	}()
}

// PprofAddrFromEnv returns LOGSERVE_PPROF_ADDR when set.
//
// The value is trimmed so command flag defaults treat whitespace-only values
// the same as an unset address.
func PprofAddrFromEnv() string {
	return strings.TrimSpace(os.Getenv("LOGSERVE_PPROF_ADDR"))
}
