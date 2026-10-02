// SPDX-License-Identifier: FSL-1.1-ALv2

package infra

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// ClientLogin configures an unattended client using file-based credentials.
// Source credentials remain private, and never appear in an SSM command or
// on a CLI command line. Endpoint overrides avoid an in-VPC public-IP hairpin.
type ClientLogin struct {
	CLI, STS, KeyFile, CAFile, User string
	RemoteExecution, Management     string
}

// LoginClient transfers the key and trust bundle, logs in using --key FILE,
// and removes the temporary key even if login fails. The CA stays on the
// client: the profile and Bazel's --tls_certificate reference that path.
func LoginClient(ctx context.Context, h remote.Host, opts ClientLogin) (retErr error) {
	if opts.KeyFile == "" || opts.CAFile == "" {
		return fmt.Errorf("client login needs key and CA file paths")
	}
	sep := "/"
	if h.OS() == remote.Windows {
		sep = `\`
	}
	key := strings.TrimRight(h.WorkDir(), `/\`) + sep + "secrets" + sep + "e2e.key"
	ca := strings.TrimRight(h.WorkDir(), `/\`) + sep + "secrets" + sep + "ca.pem"
	// Cleanup is registered BEFORE either transfer: a CA/transport failure
	// can occur before the remote login script has installed its own trap.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
		script := "rm -f " + q(key) + " " + q(key+".part")
		if h.OS() == remote.Windows {
			q = func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
			script = "$ErrorActionPreference='Stop'; foreach ($p in @(" + q(key) + ", " + q(key+".part") + ")) { if (Test-Path -LiteralPath $p) { Remove-Item -LiteralPath $p -Force -ErrorAction Stop }; if (Test-Path -LiteralPath $p) { throw 'private client key still present' } }"
		}
		r, err := h.Run(cleanup, script, remote.Opts{})
		if err == nil {
			err = r.Err()
		}
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("temporary client key cleanup: %w", err))
		}
	}()
	if err := remote.PutPrivate(ctx, h, opts.KeyFile, key, opts.User); err != nil {
		return err
	}
	if err := remote.PutPrivate(ctx, h, opts.CAFile, ca, opts.User); err != nil {
		return err
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	call := ""
	if h.OS() == remote.Windows {
		quote = func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
		call = "& "
	}
	cli := call + quote(opts.CLI)
	caArg := ca
	if h.OS() == remote.Windows {
		caArg = strings.ReplaceAll(caArg, `\`, "/")
	}
	login := fmt.Sprintf("%s login %s --key %s --ca-file %s --credential-store=file", cli, quote(opts.STS), quote(key), quote(caArg))
	var script string
	if h.OS() == remote.Windows {
		script = "$ErrorActionPreference='Stop'\ntry {\n" + login + "\nif ($LASTEXITCODE) { exit $LASTEXITCODE }\n"
		for _, kv := range [][2]string{{"remote-executor", opts.RemoteExecution}, {"management", opts.Management}} {
			if kv[1] != "" {
				script += cli + " config set " + kv[0] + " " + quote(kv[1]) + "\nif ($LASTEXITCODE) { exit $LASTEXITCODE }\n"
			}
		}
		script += "} finally { Remove-Item -Force -ErrorAction SilentlyContinue " + quote(key) + " }\n"
	} else {
		script = "set -eu\ntrap " + quote("rm -f "+quote(key)) + " EXIT\n" + login + "\n"
		for _, kv := range [][2]string{{"remote-executor", opts.RemoteExecution}, {"management", opts.Management}} {
			if kv[1] != "" {
				script += cli + " config set " + kv[0] + " " + quote(kv[1]) + "\n"
			}
		}
	}
	r, err := h.Run(ctx, script, remote.Opts{User: opts.User})
	if err != nil {
		return err
	}
	return r.Err()
}
