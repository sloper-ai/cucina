// SPDX-License-Identifier: FSL-1.1-ALv2

// Package greeter implements the build-system smoke-test service (R-BUILD-1).
package greeter

import (
	"context"

	"github.com/sloper-ai/cucina/tools/hello/hellopb"
)

// Server implements hellopb.GreeterServer.
type Server struct {
	hellopb.UnimplementedGreeterServer
}

// SayHello greets req.Name, or "world" when it is empty.
func (Server) SayHello(_ context.Context, req *hellopb.HelloRequest) (*hellopb.HelloReply, error) {
	name := req.GetName()
	if name == "" {
		name = "world"
	}
	return &hellopb.HelloReply{Message: "hello, " + name}, nil
}
