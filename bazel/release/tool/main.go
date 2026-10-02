// SPDX-License-Identifier: FSL-1.1-ALv2

// Command cucina-release builds and checks Cucina's release artifacts (R-OPS-7, R-BUILD-3;
// ADR 0150, ADR 0151). The rules in //bazel/release run it inside Bazel actions; the release
// workflow and release/*.sh run it with `bazel run`. It never uses the network and never
// publishes anything.
//
//	buildinfo  [--stable-status F] --out-json F [--out-env F]
//	oci-meta   --buildinfo F --title T --description D [--base-name N --base-layout DIR]
//	           --out-labels F --out-created F --out-tags F
//	wrap-oci   --layout DIR --out DIR (preserve the release OCI index envelope)
//	archive    --buildinfo F --format tar.gz|zip --top TEMPLATE --exe NAME=PATH
//	           [--link NAME]... [--doc NAME=PATH]... --out F
//	chart      --buildinfo F --helm PATH --chart-root DIR [--image-layout DIR] --out F FILE...
//	dist       --buildinfo F --out DIR [--file TEMPLATE=PATH]... [--tree DIR]... [--meta NAME=PATH]...
//	finalize   --out DIR [--formula-template F] IN_DIR...
//	verify     --dir DIR [--expect GROUP]... [--oci NAME=DIR]... [--helm PATH] [--exec-native]
//
// Templates expand {version} (SemVer), {core} (MAJOR.MINOR.PATCH) and {core_dashed}
// (MAJOR-MINOR-PATCH, the macOS package asset naming of ADR 0753).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "cucina-release: %v\n", err)
		os.Exit(1)
	}
}

type command struct {
	summary string
	run     func(args []string, stdout io.Writer) error
}

var commands = map[string]command{
	"buildinfo": {"resolve the release version from Bazel's workspace status", runBuildInfo},
	"oci-meta":  {"write OCI image labels, creation time and tags", runOCIMeta},
	"wrap-oci":  {"preserve the release OCI index envelope without changing image blobs", runWrapOCI},
	"archive":   {"write a deterministic tar.gz or zip of a binary", runArchive},
	"chart":     {"package the Helm chart for a release", runChart},
	"dist":      {"stage release assets under their final names", runDist},
	"finalize":  {"merge dists, write SHA256SUMS and the Homebrew formula", runFinalize},
	"verify":    {"check a release directory (versions, checksums, contents)", runVerify},
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		names := make([]string, 0, len(commands))
		for name := range commands {
			names = append(names, name)
		}
		sort.Strings(names)
		var b strings.Builder
		b.WriteString("usage: cucina-release <command> [flags]\n\ncommands:\n")
		for _, name := range names {
			fmt.Fprintf(&b, "  %-10s %s\n", name, commands[name].summary)
		}
		_, err := io.WriteString(stdout, b.String())
		if len(args) == 0 {
			return errors.New("missing command")
		}
		return err
	}
	cmd, ok := commands[args[0]]
	if !ok {
		return fmt.Errorf("unknown command %q (try --help)", args[0])
	}
	if err := cmd.run(args[1:], stdout); err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	return nil
}

// newFlags returns a flag set that reports errors instead of exiting.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// multiFlag collects a repeated string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// pairs splits NAME=VALUE flag values.
func pairs(values []string) ([][2]string, error) {
	out := make([][2]string, 0, len(values))
	for _, v := range values {
		name, value, ok := strings.Cut(v, "=")
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("expected NAME=VALUE, got %q", v)
		}
		out = append(out, [2]string{name, value})
	}
	return out, nil
}

func required(fs *flag.FlagSet, names ...string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, n := range names {
		if !set[n] {
			return fmt.Errorf("--%s is required", n)
		}
	}
	return nil
}
