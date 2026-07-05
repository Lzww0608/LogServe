// Package rpcauth centralizes the lightweight gRPC bearer-token checks used by
// LogServe's internal control, log, worker, and web processes. It protects unary
// RPC metadata only; transport encryption and stream interceptors are outside
// this package's scope.
package rpcauth

import (
	"context"
	"crypto/subtle"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// EnvAPIToken and authorizationKey define the shared environment and metadata
// names for LogServe's internal bearer-token RPC authentication.
const (
	// EnvAPIToken is the process-wide backend RPC token shared by logd, control,
	// workers, webapi, and CLI clients. It is distinct from web UI role tokens.
	EnvAPIToken = "LOGSERVE_API_TOKEN"
	// authorizationKey is the incoming/outgoing gRPC metadata key used for the
	// bearer token. gRPC metadata keys are case-insensitive and stored lowercase.
	authorizationKey = "authorization"
)

// ServerOptionsFromEnv builds server options from EnvAPIToken.
//
// When the environment value is empty or whitespace-only, authentication is
// disabled and no server option is returned.
func ServerOptionsFromEnv() []grpc.ServerOption {
	return ServerOptions(os.Getenv(EnvAPIToken))
}

// ServerOptions returns gRPC server options that enforce the provided token.
//
// The token is trimmed before use. An empty token deliberately returns nil so
// local development and tests can run without configuring RPC authentication.
// The returned option installs only a unary interceptor; streaming RPCs would
// need an explicit stream interceptor if this package starts serving them.
func ServerOptions(token string) []grpc.ServerOption {
	// Normalize once at option construction so callers can pass raw env values
	// without changing the runtime check performed by the interceptor.
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	return []grpc.ServerOption{grpc.UnaryInterceptor(UnaryServerInterceptor(token))}
}

// UnaryServerInterceptor rejects unary RPCs that do not present the configured token.
//
// If token is empty after trimming, the interceptor becomes a pass-through. On
// failure, the handler is not called and the client receives Unauthenticated.
func UnaryServerInterceptor(token string) grpc.UnaryServerInterceptor {
	token = strings.TrimSpace(token)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if token == "" {
			// Empty token is the explicit auth-disabled mode used by local tests and
			// developer processes; do not require clients to send placeholder metadata.
			return handler(ctx, req)
		}
		if !authorized(ctx, token) {
			return nil, status.Error(codes.Unauthenticated, "missing or invalid logserve api token")
		}
		return handler(ctx, req)
	}
}

// InsecureDialOptionsFromEnv builds client dial options from EnvAPIToken.
//
// It mirrors ServerOptionsFromEnv so command-line clients and internal services
// use the same shared token source.
func InsecureDialOptionsFromEnv() []grpc.DialOption {
	return InsecureDialOptions(os.Getenv(EnvAPIToken))
}

// InsecureDialOptions returns gRPC dial options for internal plaintext RPCs.
//
// Transport credentials are intentionally insecure here; this helper only adds
// application-level bearer metadata when token is non-empty. Callers that need
// transport security must use a different dial option set.
func InsecureDialOptions(token string) []grpc.DialOption {
	// This project uses local/plaintext gRPC channels; bearer metadata is an
	// application-level guard, not transport encryption.
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	token = strings.TrimSpace(token)
	if token != "" {
		opts = append(opts, grpc.WithUnaryInterceptor(UnaryClientInterceptor(token)))
	}
	return opts
}

// UnaryClientInterceptor attaches the configured bearer token to unary RPCs.
//
// Empty tokens are pass-through so the same dial helper can be used when the
// server is running without RPC authentication.
func UnaryClientInterceptor(token string) grpc.UnaryClientInterceptor {
	token = strings.TrimSpace(token)
	return func(ctx context.Context, method string, req any, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if token != "" {
			// Append instead of replacing so caller-provided metadata such as request IDs
			// survives while the auth header is added for this unary RPC.
			ctx = metadata.AppendToOutgoingContext(ctx, authorizationKey, "Bearer "+token)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// authorized reports whether ctx carries a matching authorization metadata value.
//
// Multiple metadata values are accepted because proxies or clients may append
// headers. Values may be either "Bearer <token>" or a bare token; the latter is
// kept as a small compatibility allowance for non-HTTP-style gRPC callers.
func authorized(ctx context.Context, token string) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		// Missing metadata is treated the same as a bad token so callers do not get
		// different error details for probing authentication state.
		return false
	}
	for _, value := range md.Get(authorizationKey) {
		// Check every value because gRPC metadata can carry repeated authorization
		// entries after interceptors or proxies append their own metadata.
		presented := strings.TrimSpace(value)
		if strings.HasPrefix(strings.ToLower(presented), "bearer ") {
			// Strip using the original string so token case is preserved even
			// though the prefix check is case-insensitive.
			presented = strings.TrimSpace(presented[len("bearer "):])
		}
		if constantTimeEqual(presented, token) {
			return true
		}
	}
	return false
}

// constantTimeEqual compares equal-length tokens without data-dependent timing.
//
// Length mismatches are rejected before the byte comparison; equal-length inputs
// use subtle.ConstantTimeCompare to avoid data-dependent match timing.
func constantTimeEqual(a, b string) bool {
	// Rejecting length mismatches leaks only token length and avoids allocating or
	// padding before the constant-time byte comparison.
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
