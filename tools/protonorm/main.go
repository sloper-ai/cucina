// SPDX-License-Identifier: FSL-1.1-ALv2

// Command protonorm normalises the compiler version that protoc-gen-go and
// protoc-gen-go-grpc record in generated headers to "(unknown)", exactly as
// `buf generate` writes it. Checked-in *.pb.go files then do not depend on the
// protoc release, and Bazel (cucina_proto_go) and buf produce identical bytes.
//
// Usage: protonorm <in.go> <out.go>
package main

import (
	"fmt"
	"os"
	"regexp"
)

// Matches "// \tprotoc        v7.36.2" (protoc-gen-go) and
// "// - protoc             v7.36.2" (protoc-gen-go-grpc).
var protocVersion = regexp.MustCompile(`(?m)^(//[ \t]+(?:- )?protoc[ \t]+)v[^\s]+$`)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: protonorm <in.go> <out.go>")
		os.Exit(2)
	}
	in, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out := protocVersion.ReplaceAll(in, []byte("${1}(unknown)"))
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
