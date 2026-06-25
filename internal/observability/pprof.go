package observability

import (
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	defaultMutexProfileFraction = 10
	defaultBlockProfileRate     = 10000
)

// EnableRuntimeProfiles turns on mutex and block profiling samples used by
// /debug/pprof/mutex and /debug/pprof/block.
func EnableRuntimeProfiles() {
	mutexFraction := defaultMutexProfileFraction
	if v := os.Getenv("LOGSERVE_MUTEX_PROFILE_FRACTION"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			mutexFraction = parsed
		}
	}
	blockRate := defaultBlockProfileRate
	if v := os.Getenv("LOGSERVE_BLOCK_PROFILE_RATE"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			blockRate = parsed
		}
	}
	runtime.SetMutexProfileFraction(mutexFraction)
	runtime.SetBlockProfileRate(blockRate)
}

// StartDebugServer serves runtime profiles on addr when non-empty.
// Typical endpoints: /debug/pprof/profile, /heap, /mutex, /block.
func StartDebugServer(addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	EnableRuntimeProfiles()
	go func() {
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
func PprofAddrFromEnv() string {
	return strings.TrimSpace(os.Getenv("LOGSERVE_PPROF_ADDR"))
}
