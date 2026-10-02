// SPDX-License-Identifier: FSL-1.1-ALv2

package metrics_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/hostd/metrics"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Guards: T13 measured worker RSS — only a stable, root-owned launchd worker's
// real resident pages count; missing, failed, ambiguous or reused PIDs stay absent.
func TestWorkerResidentMemory(t *testing.T) {
	for _, tc := range []struct {
		name, uid, rss, program                         string
		badPID, duplicatePID, stopped, replaced, failed bool
		want                                            uint64
	}{
		{name: "observed guest shape", uid: "0", rss: "56480", want: 56480 * 1024},
		{name: "zero is unavailable", uid: "0", rss: "0"},
		{name: "negative is unavailable", uid: "0", rss: "-1"},
		{name: "overflow is unavailable", uid: "0", rss: "18446744073709551615"},
		{name: "wrong process user", uid: "600", rss: "16"},
		{name: "wrong executable", uid: "0", rss: "16", program: "/bin/sleep"},
		{name: "ps reports another pid", uid: "0", rss: "16", badPID: true},
		{name: "ambiguous job pid", uid: "0", rss: "16", duplicatePID: true},
		{name: "job not running", uid: "0", rss: "16", stopped: true},
		{name: "job restarts during probe", uid: "0", rss: "16", replaced: true},
		{name: "ps query fails", uid: "0", rss: "16", failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := fakes.NewClock(time.Unix(0, 0))
			ex := fakes.NewExec(clock, fakes.NewRand(1))
			pid := 321
			ex.Handle("/usr/bin/sudo", func(_ context.Context, c ports.Command) (ports.ExecResult, error) {
				args := c.Args
				if len(args) < 3 || args[0] != "-n" || args[1] != "--" {
					return ports.ExecResult{ExitCode: 1}, nil
				}
				args = args[2:]
				switch args[0] {
				case "/bin/launchctl":
					if len(args) != 3 || args[1] != "print" || args[2] != "system/ai.sloper.cucina.bb-worker" {
						return ports.ExecResult{ExitCode: 1}, nil
					}
					state := "running"
					if tc.stopped {
						state = "waiting"
					}
					data := fmt.Sprintf("system/ai.sloper.cucina.bb-worker = {\n\tstate = %s\n\tprogram = /usr/local/cucina/bin/bb_worker\n\tpid = %d\n", state, pid)
					if tc.duplicatePID {
						data += "\tpid = 444\n"
					}
					return ports.ExecResult{Stdout: []byte(data + "}\n")}, nil
				case "/bin/ps":
					flag := func(name string) string {
						for i := 1; i+1 < len(args); i++ {
							if args[i] == name {
								return args[i+1]
							}
						}
						return ""
					}
					if tc.failed || flag("-p") != strconv.Itoa(pid) || flag("-o") != "pid=,uid=,rss=,comm=" {
						return ports.ExecResult{ExitCode: 1}, nil
					}
					observed := pid
					if tc.badPID {
						observed++
					}
					program := tc.program
					if program == "" {
						program = "/usr/local/cucina/bin/bb_worker"
					}
					out := fmt.Sprintf("%d %s %s %s\n", observed, tc.uid, tc.rss, program)
					if tc.replaced {
						pid++
					}
					return ports.ExecResult{Stdout: []byte(out)}, nil
				default:
					return ports.ExecResult{ExitCode: 1, Stderr: []byte(strings.Join(args, " "))}, nil
				}
			})
			got, err := metrics.WorkerResidentMemory(t.Context(), ex.Run)
			if tc.want == 0 {
				require.ErrorIs(t, err, metrics.ErrWorkerRSSUnavailable)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
