// SPDX-License-Identifier: FSL-1.1-ALv2

// Command hello serves the smoke-test Greeter API. It is the payload of the
// OCI image proof (cucina_go_image) and is not shipped.
package main

import (
	"flag"
	"log/slog"
	"net"
	"os"

	"google.golang.org/grpc"

	"github.com/sloper-ai/cucina/tools/hello/greeter"
	"github.com/sloper-ai/cucina/tools/hello/hellopb"
)

func main() {
	addr := flag.String("listen", ":50051", "gRPC listen address")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "addr", *addr, "err", err)
		os.Exit(1)
	}
	srv := grpc.NewServer()
	hellopb.RegisterGreeterServer(srv, greeter.Server{})
	log.Info("serving", "addr", lis.Addr().String())
	if err := srv.Serve(lis); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
