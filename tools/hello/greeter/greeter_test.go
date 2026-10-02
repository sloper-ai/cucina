// SPDX-License-Identifier: FSL-1.1-ALv2

package greeter_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/sloper-ai/cucina/tools/hello/greeter"
	"github.com/sloper-ai/cucina/tools/hello/hellopb"
)

// TestSayHello guards R-BUILD-1: the checked-in go_proto/go_grpc_v2 output
// compiles and round-trips through an in-process gRPC server.
func TestSayHello(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	hellopb.RegisterGreeterServer(srv, greeter.Server{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := hellopb.NewGreeterClient(conn)

	for _, tc := range []struct{ name, want string }{
		{name: "", want: "hello, world"},
		{name: "cucina", want: "hello, cucina"},
	} {
		got, err := client.SayHello(t.Context(), &hellopb.HelloRequest{Name: tc.name})
		if err != nil {
			t.Fatalf("SayHello(%q): %v", tc.name, err)
		}
		if got.GetMessage() != tc.want {
			t.Errorf("SayHello(%q) = %q, want %q", tc.name, got.GetMessage(), tc.want)
		}
	}
}
