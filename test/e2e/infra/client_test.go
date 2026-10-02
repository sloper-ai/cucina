// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build !windows

package infra_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// Guards T0/§12: an ambient profile for a different cluster is not used as
// evidence. An explicit private bootstrap can bind the correct profile, and
// setup failure cannot be mistaken for a successful bind or leak its output.
func TestLocalProfileBinding(t *testing.T) {
	t.Setenv("CUCINA_PROFILE", "")
	for _, mode := range []string{"wrong-cluster", "bound", "bootstrap", "bootstrap-failed"} {
		t.Run(mode, func(t *testing.T) {
			exec := fakes.NewExec(fakes.NewClock(time.Unix(0, 0)), fakes.NewRand(1))
			e := &harness.Env{Cucinactl: map[string]string{runtime.GOOS: "/tools/cucinactl"}, Endpoints: harness.Endpoints{STS: "https://correct.example.test:8443", RemoteExecution: "grpcs://correct.example.test:443", Management: "correct.example.test:8444", CAFile: "/private/ca.pem"}, Kubernetes: &harness.KubeEnv{}}
			bound := mode == "bound"
			exec.Handle("/tools/cucinactl", func(context.Context, ports.Command) (ports.ExecResult, error) {
				url := "https://wrong.example.test:8443"
				if bound {
					url = e.Endpoints.STS
				}
				b, err := json.Marshal(map[string]any{"profiles": []map[string]any{{"name": "campaign", "current": true, "url": url, "remote_executor": e.Endpoints.RemoteExecution, "management": e.Endpoints.Management, "ca_file": e.Endpoints.CAFile}}})
				return ports.ExecResult{Stdout: b}, err
			})
			if mode == "bootstrap" || mode == "bootstrap-failed" {
				e.Kubernetes.BootstrapCLI = []string{"/private/bootstrap"}
				exec.Handle("/private/bootstrap", func(context.Context, ports.Command) (ports.ExecResult, error) {
					if mode == "bootstrap-failed" {
						return ports.ExecResult{ExitCode: 1, Stderr: []byte("secret-sentinel")}, nil
					}
					bound = true
					return ports.ExecResult{}, nil
				})
			}
			err := (&infra.Services{Env: e, Exec: exec}).BootstrapLocalProfile(context.Background())
			switch mode {
			case "bound", "bootstrap":
				require.NoError(t, err)
			case "wrong-cluster":
				require.Equal(t, harness.StatusSkip, harness.Classify(err))
			case "bootstrap-failed":
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret-sentinel")
			}
		})
	}
}

// Guards R-AUTH-8/§12: client login takes key and CA FILES, never key contents
// in argv, preserves the CA for Bazel and deletes the temporary key on failure.
func TestClientLoginFiles(t *testing.T) {
	root := t.TempDir()
	key, ca := filepath.Join(root, "writer.key"), filepath.Join(root, "ca.pem")
	require.NoError(t, os.WriteFile(key, []byte("fixture-not-a-real-key"), 0o600))
	require.NoError(t, os.WriteFile(ca, []byte("fixture-not-a-real-certificate"), 0o600))
	cli := filepath.Join(root, "fake-cli")
	require.NoError(t, os.WriteFile(cli, []byte(`#!/bin/sh
set -eu
case $1 in
login)
  shift 2
  key= ca=
  while [ $# -gt 0 ]; do
    case $1 in
    --key) key=$2; shift ;;
    --ca-file) ca=$2; shift ;;
    --credential-store=file) : ;;
    *) exit 90 ;;
    esac
    shift
  done
  test -f "$key" && test -f "$ca"
  test "$(cat "$key")" = fixture-not-a-real-key
  test "$(cat "$ca")" = fixture-not-a-real-certificate
  exit "${LOGIN_EXIT:-0}"
  ;;
config) test "$2" = set; case $3 in remote-executor) test "$4" = grpcs://192.0.2.9:443 ;; management) test "$4" = 192.0.2.9:8444 ;; *) exit 91 ;; esac ;;
*) exit 92 ;;
esac
`), 0o755))
	for _, exit := range []string{"0", "5", "ca-transfer-failed"} {
		t.Run(exit, func(t *testing.T) {
			t.Setenv("LOGIN_EXIT", exit)
			work := t.TempDir()
			h := remote.NewLocal("local", work)
			caFile := ca
			if exit == "ca-transfer-failed" {
				caFile = filepath.Join(root, "missing-ca")
			}
			err := infra.LoginClient(context.Background(), h, infra.ClientLogin{CLI: cli, STS: "https://cucina.example.test:8443", KeyFile: key, CAFile: caFile, RemoteExecution: "grpcs://192.0.2.9:443", Management: "192.0.2.9:8444"})
			if exit == "0" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			_, err = os.Stat(filepath.Join(work, "secrets/e2e.key"))
			require.True(t, os.IsNotExist(err))
			if exit == "ca-transfer-failed" {
				return
			}
			b, err := os.ReadFile(filepath.Join(work, "secrets/ca.pem"))
			require.NoError(t, err)
			require.Equal(t, "fixture-not-a-real-certificate", string(b))
		})
	}
}
