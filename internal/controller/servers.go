// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// shutdownGrace bounds graceful server shutdown (in-flight RPCs, streams).
const shutdownGrace = 20 * time.Second

// grpcServer runs one gRPC listener on every replica (not leader-only).
type grpcServer struct {
	name Listener
	addr string
	srv  *grpc.Server
	log  *slog.Logger
	// services counts registered services; an empty server is not started.
	services int
}

func newGRPCServer(name Listener, addr string, tlsCfg *tls.Config, maxMsg int, log *slog.Logger) *grpcServer {
	opts := []grpc.ServerOption{
		// hostd pings every 10 s, clients every 30 s or more: never answer with
		// GOAWAY too_many_pings (R-CP-5).
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 2 * time.Minute, Timeout: 20 * time.Second}),
		grpc.MaxRecvMsgSize(maxMsg),
		grpc.MaxSendMsgSize(maxMsg),
	}
	if tlsCfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	return &grpcServer{name: name, addr: addr, srv: grpc.NewServer(opts...), log: log}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: servers run on every replica.
func (g *grpcServer) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable.
func (g *grpcServer) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", g.addr)
	if err != nil {
		return err
	}
	g.log.Info("serving gRPC", "listener", g.name, "addr", lis.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- g.srv.Serve(lis) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	stopped := make(chan struct{})
	go func() { g.srv.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(shutdownGrace):
		g.srv.Stop()
	}
	return nil
}

// httpsServer runs an HTTPS listener (the STS) on every replica.
type httpsServer struct {
	name Listener
	srv  *http.Server
	log  *slog.Logger
}

func newHTTPSServer(name Listener, addr string, h http.Handler, tlsCfg *tls.Config, log *slog.Logger) *httpsServer {
	return &httpsServer{name: name, log: log, srv: &http.Server{
		Addr:              addr,
		Handler:           h,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (h *httpsServer) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable.
func (h *httpsServer) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", h.srv.Addr)
	if err != nil {
		return err
	}
	h.log.Info("serving HTTPS", "listener", h.name, "addr", lis.Addr().String())
	errc := make(chan error, 1)
	go func() {
		if h.srv.TLSConfig != nil {
			errc <- h.srv.ServeTLS(lis, "", "")
		} else {
			errc <- h.srv.Serve(lis)
		}
	}()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	return h.srv.Shutdown(sctx)
}

// runnerAdapter turns a Runner into a manager.Runnable.
type runnerAdapter struct {
	name   string
	r      Runner
	leader bool
}

func (a runnerAdapter) Start(ctx context.Context) error { return a.r.Run(ctx) }
func (a runnerAdapter) NeedLeaderElection() bool        { return a.leader }
