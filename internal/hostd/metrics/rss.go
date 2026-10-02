// SPDX-License-Identifier: FSL-1.1-ALv2

package metrics

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/hostd/guest"
	"github.com/sloper-ai/cucina/internal/ports"
)

// ErrWorkerRSSUnavailable means no unambiguous, fresh measurement was obtained.
// Callers omit the metric; zero or Go heap usage are not RSS substitutes.
var ErrWorkerRSSUnavailable = errors.New("worker resident memory unavailable")

const workerProgram = "/usr/local/cucina/bin/bb_worker"

// WorkerResidentMemory measures the root-owned worker of the known system
// launchd job through the caller's scoped GuestExec. It never searches global
// process names. Verify launchd identity before and after ps to reject restarts
// and ambiguous/PID-reused observations. Darwin ps reports RSS in KiB.
func WorkerResidentMemory(ctx context.Context, exec func(context.Context, ports.Command) (ports.ExecResult, error)) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	job, err := exec(ctx, guest.WorkerStatusCmd())
	if err != nil {
		return 0, ErrWorkerRSSUnavailable
	}
	pid, ok := workerPID(job)
	if !ok {
		return 0, ErrWorkerRSSUnavailable
	}
	res, err := exec(ctx, ports.Command{Path: "/usr/bin/sudo", Args: []string{
		"-n", "--", "/bin/ps", "-p", strconv.Itoa(pid), "-o", "pid=,uid=,rss=,comm=",
	}})
	if err != nil || res.ExitCode != 0 || len(res.Stdout) > 4096 {
		return 0, ErrWorkerRSSUnavailable
	}
	fields := strings.Fields(string(res.Stdout))
	if len(fields) != 4 || fields[0] != strconv.Itoa(pid) || fields[1] != "0" || fields[3] != workerProgram {
		return 0, ErrWorkerRSSUnavailable
	}
	kib, err := strconv.ParseUint(fields[2], 10, 54) // multiplying by 1024 cannot overflow uint64
	if err != nil || kib == 0 {
		return 0, ErrWorkerRSSUnavailable
	}
	after, err := exec(ctx, guest.WorkerStatusCmd())
	if err != nil {
		return 0, ErrWorkerRSSUnavailable
	}
	current, ok := workerPID(after)
	if !ok || current != pid || ctx.Err() != nil {
		return 0, ErrWorkerRSSUnavailable
	}
	return kib * 1024, nil
}

func workerPID(res ports.ExecResult) (int, bool) {
	if res.ExitCode != 0 || len(res.Stdout) > 16<<10 {
		return 0, false
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		// Only the top-level job's keys count. Nested coalition/process entries
		// cannot supply a PID, program or state for this job.
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, value, found := strings.Cut(line[1:], " = ")
		if !found || (key != "pid" && key != "program" && key != "state") {
			continue
		}
		if _, duplicate := fields[key]; duplicate {
			return 0, false
		}
		fields[key] = value
	}
	if fields["state"] != "running" || fields["program"] != workerProgram {
		return 0, false
	}
	pid, err := strconv.ParseInt(fields["pid"], 10, 32)
	return int(pid), err == nil && pid > 0
}
