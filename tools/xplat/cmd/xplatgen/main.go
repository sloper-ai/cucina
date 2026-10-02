// SPDX-License-Identifier: FSL-1.1-ALv2

// Command xplatgen validates platforms/pools.json + platforms/targets.json and writes the
// generated files of the @cucina_platforms module (bazel/platforms). Bazel runs it through
// //tools/xplat:generated; `bazel run //tools/xplat:update` copies the result into the tree.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sloper-ai/cucina/tools/xplat"
)

func main() {
	pools := flag.String("pools", "platforms/pools.json", "path of platforms/pools.json")
	targets := flag.String("targets", "platforms/targets.json", "path of platforms/targets.json")
	out := flag.String("out", "", "directory receiving the generated module files (the module root layout)")
	segment := flag.String("segment", "", "path of the root-module segment (default: <out>/"+xplat.ModuleSegment+")")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "xplatgen: -out is required")
		os.Exit(2)
	}
	c, err := xplat.Load(*pools, *targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xplatgen: invalid platform catalog:\n%v\n", err)
		os.Exit(1)
	}
	for path, content := range c.Generate() {
		dst := filepath.Join(*out, filepath.FromSlash(path))
		if path == xplat.ModuleSegment && *segment != "" {
			dst = *segment
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "xplatgen: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "xplatgen: %v\n", err)
			os.Exit(1)
		}
	}
}
