// SPDX-License-Identifier: FSL-1.1-ALv2

package privdrop_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/hostd/privdrop"
	"github.com/sloper-ai/cucina/internal/ports"
)

const self = "/usr/local/cucina/bin/cucina-hostd"

var cucina = privdrop.User{Name: "cucina", UID: 502, GID: 20, Groups: []uint32{12, 61}, Home: "/Users/cucina"}

// TestWrap guards R-MAC-2 (tart runs as the dedicated non-root user inside its
// login session) and R-MAC-5 (registry credentials only in the environment).
func TestWrap(t *testing.T) {
	pull := ports.Command{
		Path:  "/usr/local/cucina/tart.app/Contents/MacOS/tart",
		Args:  []string{"pull", "ghcr.io/sloper-ai/cucina-worker-macos:27.0"},
		Env:   privdrop.BaseEnv(cucina, "TART_REGISTRY_USERNAME=bot", "TART_REGISTRY_PASSWORD=hunter2"),
		RunAs: cucina.RunAs(),
	}
	tests := []struct {
		name     string
		mode     privdrop.Mode
		cmd      ports.Command
		wantPath string
		wantArgs []string
		wantCred bool
		wantErr  string
	}{
		{name: "asuser trampoline", mode: privdrop.ModeAsUser, cmd: pull, wantPath: "/bin/launchctl",
			wantArgs: []string{"asuser", "502", self, "drop-exec", "--uid", "502", "--gid", "20", "--groups", "12,61", "--",
				"/usr/local/cucina/tart.app/Contents/MacOS/tart", "pull", "ghcr.io/sloper-ai/cucina-worker-macos:27.0"}},
		{name: "setuid fallback", mode: privdrop.ModeSetuid, cmd: pull, wantPath: pull.Path, wantArgs: pull.Args, wantCred: true},
		{name: "user mode runs directly", mode: privdrop.ModeNone, cmd: pull, wantPath: pull.Path, wantArgs: pull.Args},
		{name: "no RunAs runs directly", mode: privdrop.ModeAsUser, cmd: ports.Command{Path: "/usr/bin/true"},
			wantPath: "/usr/bin/true"},
		{name: "root target refused", mode: privdrop.ModeAsUser,
			cmd: ports.Command{Path: "/x", RunAs: &ports.RunAs{User: "root"}}, wantErr: "uid 0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := privdrop.Wrap(tc.cmd, tc.mode, self, cucina.Groups)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantPath, spec.Path)
			require.Equal(t, tc.wantArgs, spec.Args)
			require.Equal(t, tc.wantCred, spec.Credential != nil)
			require.Equal(t, tc.cmd.Env, spec.Env)
			require.NotContains(t, strings.Join(spec.Args, " "), "hunter2", "secrets must never be in argv")
			if tc.mode == privdrop.ModeAsUser && tc.cmd.RunAs != nil {
				// The trampoline parses exactly what Wrap produced.
				tr, err := privdrop.ParseTrampoline(spec.Args[4:])
				require.NoError(t, err)
				require.Equal(t, privdrop.TrampolineArgs{UID: 502, GID: 20, Groups: []uint32{12, 61},
					Argv: append([]string{tc.cmd.Path}, tc.cmd.Args...)}, tr)
			}
		})
	}
	require.Contains(t, pull.Env, "HOME=/Users/cucina")
}

// TestParseTrampolineRejects guards the trampoline against running as root or
// a relative program.
func TestParseTrampolineRejects(t *testing.T) {
	for _, args := range [][]string{
		{"--uid", "0", "--gid", "0", "--", "/bin/sh"},
		{"--uid", "502", "--", "/bin/sh"},
		{"--uid", "502", "--gid", "20", "--", "sh"},
		{"--uid", "502", "--gid", "20"},
		{"--uid", "x", "--gid", "20", "--", "/bin/sh"},
	} {
		_, err := privdrop.ParseTrampoline(args)
		require.Error(t, err, "%v", args)
	}
}
