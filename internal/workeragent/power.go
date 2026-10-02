// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"context"
	"log/slog"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Poweroff powers the machine off. On EC2 the launch sets
// InstanceInitiatedShutdownBehavior=terminate, so this terminates the
// instance and releases every volume (R-POOL-1/-7).
type Poweroff interface {
	PowerOff(ctx context.Context, reason string) error
}

// ExecPoweroff runs the OS power-off command.
type ExecPoweroff struct {
	Exec    ports.Exec
	Command []string
}

// DefaultPoweroffCommand returns the power-off command of a GOOS.
func DefaultPoweroffCommand(goos string) []string {
	switch goos {
	case "windows":
		return []string{"shutdown.exe", "/s", "/t", "0", "/f", "/d", "p:4:1", "/c", "cucina-worker-agent"}
	case "darwin":
		return []string{"/sbin/shutdown", "-h", "now"}
	default:
		return []string{"systemctl", "poweroff", "--no-block"}
	}
}

// PowerOff implements Poweroff.
func (p ExecPoweroff) PowerOff(ctx context.Context, _ string) error {
	_, err := run(ctx, p.Exec, ports.Command{Path: p.Command[0], Args: p.Command[1:]})
	return err
}

// LogPoweroff only logs (--no-poweroff for debugging, and the default on
// macOS where hostd owns the VM lifecycle).
type LogPoweroff struct{ Log *slog.Logger }

// PowerOff implements Poweroff.
func (p LogPoweroff) PowerOff(_ context.Context, reason string) error {
	p.Log.Warn("poweroff suppressed (--no-poweroff)", "event", "poweroff.suppressed", "reason", reason)
	return nil
}

// WorkerControl asks bb_worker to stop taking work and finish its current
// action (SIGTERM on Linux through systemd, CTRL_C on Windows through shawl).
type WorkerControl interface {
	Drain(ctx context.Context) error
}

// ExecWorkerControl runs a service-manager command.
type ExecWorkerControl struct {
	Exec    ports.Exec
	Command []string
}

// DefaultDrainCommand returns the drain command of a GOOS for service svc.
// `systemctl stop --no-block` delivers SIGTERM (KillSignal) to bb_worker and
// prevents a Restart= from bringing it back; the unit's TimeoutStopSec bounds
// the drain. On Windows `sc.exe stop` makes shawl send CTRL_C (its
// --stop-timeout bounds the drain).
func DefaultDrainCommand(goos, svc string) []string {
	if goos == "windows" {
		return []string{"sc.exe", "stop", svc}
	}
	return []string{"systemctl", "stop", "--no-block", svc}
}

// DefaultWorkerService returns the bb_worker service name of a GOOS.
func DefaultWorkerService(goos string) string {
	if goos == "windows" {
		return "cucina-bb-worker"
	}
	return "bb-worker.service"
}

// Drain implements WorkerControl.
func (w ExecWorkerControl) Drain(ctx context.Context) error {
	_, err := run(ctx, w.Exec, ports.Command{Path: w.Command[0], Args: w.Command[1:]})
	return err
}
