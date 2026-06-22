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

const (
	EnvAPIToken      = "LOGSERVE_API_TOKEN"
	authorizationKey = "authorization"
)

func ServerOptionsFromEnv() []grpc.ServerOption {
	return ServerOptions(os.Getenv(EnvAPIToken))
}

func ServerOptions(token string) []grpc.ServerOption {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	return []grpc.ServerOption{grpc.UnaryInterceptor(UnaryServerInterceptor(token))}
}

func UnaryServerInterceptor(token string) grpc.UnaryServerInterceptor {
	token = strings.TrimSpace(token)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if token == "" {
			return handler(ctx, req)
		}
		if !authorized(ctx, token) {
			return nil, status.Error(codes.Unauthenticated, "missing or invalid logserve api token")
		}
		return handler(ctx, req)
	}
}

func InsecureDialOptionsFromEnv() []grpc.DialOption {
	return InsecureDialOptions(os.Getenv(EnvAPIToken))
}

func InsecureDialOptions(token string) []grpc.DialOption {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	token = strings.TrimSpace(token)
	if token != "" {
		opts = append(opts, grpc.WithUnaryInterceptor(UnaryClientInterceptor(token)))
	}
	return opts
}

func UnaryClientInterceptor(token string) grpc.UnaryClientInterceptor {
	token = strings.TrimSpace(token)
	return func(ctx context.Context, method string, req any, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, authorizationKey, "Bearer "+token)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func authorized(ctx context.Context, token string) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, value := range md.Get(authorizationKey) {
		presented := strings.TrimSpace(value)
		if strings.HasPrefix(strings.ToLower(presented), "bearer ") {
			presented = strings.TrimSpace(presented[len("bearer "):])
		}
		if constantTimeEqual(presented, token) {
			return true
		}
	}
	return false
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
