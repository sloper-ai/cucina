// SPDX-License-Identifier: FSL-1.1-ALv2

// Command slogen writes slo/rules.json (Prometheus recording rules) and
// slo/sloth.json (Sloth prometheus/v1 SLO spec) from the definitions in
// package slo. The chart copies both into charts/cucina/files/rules/.
//
//	go run ./slo/cmd/slogen            # from the repository root
//	go run ./slo/cmd/slogen -dir slo   # explicit output directory
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sloper-ai/cucina/slo"
)

func main() {
	dir := flag.String("dir", "slo", "output directory")
	flag.Parse()
	for name, gen := range map[string]func() ([]byte, error){"rules.json": slo.RulesFile, "sloth.json": slo.SlothSpec} {
		b, err := gen()
		if err == nil {
			err = os.WriteFile(filepath.Join(*dir, name), b, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "slogen:", err)
			os.Exit(1)
		}
	}
}
