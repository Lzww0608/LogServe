package webapi

import (
	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/rpcauth"
	"google.golang.org/grpc"
)

type Clients struct {
	controlConn *grpc.ClientConn
	logConn     *grpc.ClientConn
	Control     logservepb.ControlServiceClient
	Log         logservepb.LogServiceClient
}

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
