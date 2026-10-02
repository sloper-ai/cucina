// SPDX-License-Identifier: FSL-1.1-ALv2

// render-goldens writes internal/bbconfig's golden files into a directory. The
// Bazel genrule //internal/bbconfig:goldens_gen runs it and write_source_files
// (//internal/bbconfig:goldens) copies the result into testdata/.
//
//	render-goldens <output directory>
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sloper-ai/cucina/internal/bbconfig/bbconfigtest"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: render-goldens <output directory>")
		os.Exit(2)
	}
	goldens, err := bbconfigtest.Goldens()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, b := range goldens {
		if err := os.WriteFile(filepath.Join(os.Args[1], name), b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
