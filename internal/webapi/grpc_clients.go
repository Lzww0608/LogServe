package webapi

// This file constructs backend gRPC clients for the control plane and log
// service using the process-level API token.

import (
	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/rpcauth"
	"google.golang.org/grpc"
)

// Clients groups the control and log gRPC connections used by HTTP handlers.
// Only APIToken is passed to these clients; role tokens are not backend auth.
type Clients struct {
	controlConn *grpc.ClientConn
	logConn     *grpc.ClientConn
	Control     logservepb.ControlServiceClient
	Log         logservepb.LogServiceClient
}

// DialClients opens control-plane and log-service gRPC clients with the backend
// API token configured for this webapi process.
func DialClients(cfg Config) (*Clients, error) {
	controlConn, err := grpc.NewClient(cfg.ControlAddr, rpcauth.InsecureDialOptions(cfg.APIToken)...)
	if err != nil {
		return nil, err
	}
	logConn, err := grpc.NewClient(cfg.LogAddr, rpcauth.InsecureDialOptions(cfg.APIToken)...)
	if err != nil {
		_ = controlConn.Close()
		return nil, err
	}
	return &Clients{
		controlConn: controlConn,
		logConn:     logConn,
		Control:     logservepb.NewControlServiceClient(controlConn),
		Log:         logservepb.NewLogServiceClient(logConn),
	}, nil
}

// Close releases both gRPC connections and tolerates a nil Clients receiver.
func (c *Clients) Close() error {
	if c == nil {
		return nil
	}
	if c.logConn != nil {
		_ = c.logConn.Close()
	}
	if c.controlConn != nil {
		return c.controlConn.Close()
	}
	return nil
}
