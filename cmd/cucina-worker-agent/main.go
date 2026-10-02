// SPDX-License-Identifier: FSL-1.1-ALv2

// Command cucina-worker-agent runs on every Cucina worker VM (contracts §5.2):
//
//	bootstrap  enroll at every boot (instance identity document + CSR → mTLS
//	           certificate + WorkerSettings), prepare L1, render the Buildbarn
//	           configuration (systemd ExecStartPre / Windows service pre-start)
//	supervise  dead-man switch (R-POOL-7), Spot interruption drain, certificate
//	           hygiene and the agent's metrics (long-running companion service)
//	render     render configuration from WorkerSettings + machine facts (hostd
//	           for Tart VMs through `tart exec`, tests, debugging)
//	selftest   image smoke test (IMDS, disks, time sync, FUSE/WinFSP, binaries)
//	version    print the version
//
// Every line it logs is one JSON object on stderr (journald / SSM).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/workeragent"
	"github.com/sloper-ai/cucina/internal/workeragent/bootdata"
	"github.com/sloper-ai/cucina/internal/workeragent/hostos"
	"github.com/sloper-ai/cucina/internal/workeragent/imds"
)

// version is set at link time (-X main.version=…).
var version = "dev"

// DefaultMetricsListen is where supervise serves the agent's own metrics.
const DefaultMetricsListen = "127.0.0.1:9982"

type globalFlags struct {
	logLevel     string
	logFile      string
	imdsEndpoint string
	root         string
	noPoweroff   bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var g globalFlags
	code := 0
	root := &cobra.Command{
		Use:           "cucina-worker-agent",
		Short:         "Cucina worker agent: boot-time enrollment, configuration and dead-man switch",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	pf := root.PersistentFlags()
	pf.StringVar(&g.logLevel, "log-level", "info", "debug | info | warn | error")
	pf.StringVar(&g.logFile, "log-file", "", "also append JSON log lines to this file (stderr is captured by journald, shawl and cucina-boot.ps1)")
	pf.StringVar(&g.imdsEndpoint, "imds-endpoint", envOr("CUCINA_IMDS_ENDPOINT", imds.DefaultEndpoint), "instance metadata service endpoint")
	pf.StringVar(&g.root, "root", "", "re-root every path below this directory (development and tests)")
	pf.BoolVar(&g.noPoweroff, "no-poweroff", os.Getenv("CUCINA_AGENT_NO_POWEROFF") == "1", "log instead of powering off (debugging)")

	root.AddCommand(bootstrapCmd(&g, &code), superviseCmd(&g, &code), renderCmd(&g), selftestCmd(&g, &code), &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cucina-worker-agent %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		},
	})
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, "cucina-worker-agent:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// env bundles the production adapters.
type env struct {
	log   *slog.Logger
	paths workeragent.Paths
	fs    ports.FS
	exec  ports.Exec
	clock ports.Clock
	imds  *imds.Client
	power workeragent.Poweroff
	close func()
}

func newEnv(g *globalFlags, stderr io.Writer, cmd string) (*env, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(g.logLevel)); err != nil {
		return nil, fmt.Errorf("--log-level: %w", err)
	}
	w, closeLog := stderr, func() {}
	if g.logFile != "" {
		_ = os.MkdirAll(dirOf(g.logFile), 0o755)
		if f, err := os.OpenFile(g.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			w, closeLog = io.MultiWriter(stderr, f), func() { _ = f.Close() }
		}
	}
	log := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})).With(
		"component", "cucina-worker-agent", "version", version)
	if cmd != "" { // bootstrap and supervise add their own "cmd" attribute
		log = log.With("cmd", cmd)
	}
	paths := workeragent.DefaultPaths(runtime.GOOS)
	if g.root != "" {
		paths = paths.Under(g.root)
	}
	e := &env{log: log, paths: paths, fs: hostos.FS{}, exec: hostos.Exec{}, clock: hostos.Clock{},
		imds: imds.New(g.imdsEndpoint), close: closeLog}
	switch {
	case g.noPoweroff, runtime.GOOS == "darwin": // hostd owns the lifecycle of macOS VMs
		e.power = workeragent.LogPoweroff{Log: log}
	default:
		e.power = workeragent.ExecPoweroff{Exec: e.exec, Command: workeragent.DefaultPoweroffCommand(runtime.GOOS)}
	}
	return e, nil
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func storageFor(e *env) workeragent.Storage {
	switch runtime.GOOS {
	case "linux":
		return workeragent.LinuxStorage{Exec: e.exec, FS: e.fs, SysRoot: e.paths.SysRoot}
	case "windows":
		return workeragent.WindowsStorage{Exec: e.exec, FS: e.fs}
	default:
		return workeragent.DirStorage{}
	}
}

func bootstrapCmd(g *globalFlags, code *int) *cobra.Command {
	var (
		bootData   string
		deadline   time.Duration
		keyReaders []string
		buildUser  string
	)
	c := &cobra.Command{
		Use:   "bootstrap",
		Short: "Enroll with the controller and write certificates and Buildbarn configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := newEnv(g, cmd.ErrOrStderr(), "")
			if err != nil {
				return err
			}
			defer e.close()
			ctx, cancel := signalContext()
			defer cancel()
			bu, err := lookupBuildUser(buildUser)
			if err != nil {
				*code = 1
				e.log.Error("build user lookup failed; powering off", "event", "bootstrap.failed", "reason", "host", "error", err.Error())
				_ = e.power.PowerOff(ctx, "bootstrap: build user")
				return err
			}
			b := &workeragent.Bootstrap{
				Paths: e.paths, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Version: version,
				IMDS: e.imds,
				DialEnroller: func(bd bootdata.BootData) (workeragent.Enroller, func() error, error) {
					en, err := workeragent.DialEnroller(bd)
					if err != nil {
						return nil, nil, err
					}
					return en, en.Close, nil
				},
				Storage: storageFor(e), Renderer: workeragent.BBConfigRenderer{},
				Host: workeragent.NewHost(e.fs, e.paths.SysRoot), FS: e.fs, Clock: e.clock, Power: e.power, Log: e.log,
				BootDataFile: bootData, Deadline: deadline, KeyReaders: keyReaders, BuildUser: bu,
			}
			if err := b.Run(ctx); err != nil {
				*code = 1
				var be *workeragent.BootError
				if errors.As(err, &be) && be.Reason == workeragent.ReasonNotAWorker {
					*code = 2 // no user data: not a controller launch; the machine stays up
				}
				return err
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&bootData, "boot-data", "", "read boot data from this file instead of the EC2 user data")
	f.DurationVar(&deadline, "deadline", workeragent.DefaultBootstrapDeadline, "give up on transient IMDS/enrollment failures after this long, then power off")
	f.StringSliceVar(&keyReaders, "key-reader", nil, "Windows: extra account allowed to read the worker key (low-privilege bb_worker service account)")
	f.StringVar(&buildUser, "build-user", defaultBuildUser(), "unprivileged user build actions run as (Linux runCommandsAs); empty to run them as bb_runner's user")
	return c
}

func defaultBuildUser() string {
	if runtime.GOOS == "linux" {
		return "bbrunner"
	}
	return ""
}

// lookupBuildUser resolves the build user (pure Go /etc/passwd lookup with
// CGO_ENABLED=0). A missing user is an image defect: fail closed (R-SEC-5).
func lookupBuildUser(name string) (*workeragent.UnixUser, error) {
	if name == "" || runtime.GOOS == "windows" {
		return nil, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("build user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("build user %q: uid %q: %w", name, u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("build user %q: gid %q: %w", name, u.Gid, err)
	}
	return &workeragent.UnixUser{UID: uint32(uid), GID: uint32(gid)}, nil
}

func superviseCmd(g *globalFlags, code *int) *cobra.Command {
	var (
		metricsListen string
		workerService string
	)
	c := &cobra.Command{
		Use:     "supervise",
		Aliases: []string{"run-supervisor"},
		Short:   "Run the dead-man switch, Spot interruption handling and certificate hygiene",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := newEnv(g, cmd.ErrOrStderr(), "")
			if err != nil {
				return err
			}
			defer e.close()
			ctx, cancel := signalContext()
			defer cancel()
			metrics := workeragent.NewAgentMetrics()
			if metricsListen != "" {
				go func() {
					if err := metrics.Serve(ctx, metricsListen); err != nil {
						e.log.Warn("agent metrics endpoint failed", "event", "metrics.failed", "error", err.Error())
					}
				}()
			}
			s := &workeragent.Supervisor{
				Paths: e.paths, FS: e.fs, Clock: e.clock, Host: workeragent.NewHost(e.fs, e.paths.SysRoot), Log: e.log,
				Power:  e.power,
				Worker: workeragent.ExecWorkerControl{Exec: e.exec, Command: workeragent.DefaultDrainCommand(runtime.GOOS, workerService)},
				NewActivity: func(ws *cucinav1.WorkerSettings) workeragent.ActivityProbe {
					if ws.GetMetricsPort() == 0 {
						e.log.Error("settings.metrics_port is 0: bb_worker activity cannot be observed; the worker counts as idle",
							"event", "activity.unobservable")
					}
					return workeragent.NewMetricsActivityProbe(ws.GetMetricsPort())
				},
				NewScheduler: func(ws *cucinav1.WorkerSettings) (workeragent.SchedulerProbe, error) {
					return workeragent.NewGRPCSchedulerProbe(e.fs, ws.GetSchedulerEndpoint(), ws.GetServerName(),
						e.paths.CertFile(), e.paths.KeyFile(), e.paths.CAFile())
				},
				Metrics: metrics,
			}
			if runtime.GOOS != "darwin" {
				s.Spot = e.imds
			}
			err = s.Run(ctx)
			wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
			workeragent.WipeKeyAtShutdown(wctx, e.exec, e.fs, e.paths, e.log)
			wcancel()
			if err != nil {
				*code = 1
				if errors.Is(err, workeragent.ErrCertExpiring) {
					*code = 3
				}
				return err
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&metricsListen, "metrics-listen", DefaultMetricsListen, "serve the agent's /metrics here (empty disables)")
	f.StringVar(&workerService, "worker-service", workeragent.DefaultWorkerService(runtime.GOOS), "bb_worker service to drain on a Spot interruption notice")
	return c
}

func renderCmd(g *globalFlags) *cobra.Command {
	var settingsFile, machineFile, outDir string
	c := &cobra.Command{
		Use:   "render",
		Short: "Render worker.json, runner.json and env from WorkerSettings (protojson) and machine facts (JSON)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fs := hostos.FS{}
			sb, err := fs.ReadFile(settingsFile)
			if err != nil {
				return err
			}
			settings, err := workeragent.DecodeSettings(sb)
			if err != nil {
				return err
			}
			mb, err := fs.ReadFile(machineFile)
			if err != nil {
				return err
			}
			m, err := workeragent.DecodeMachine(mb)
			if err != nil {
				return err
			}
			if err := m.LoadCABundle(fs); err != nil {
				return err
			}
			files, err := workeragent.RenderAll(workeragent.BBConfigRenderer{}, settings, m)
			if err != nil {
				return err
			}
			if err := fs.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			for name, b := range map[string][]byte{"worker.json": files.Worker, "runner.json": files.Runner, "env": files.Env} {
				if err := fs.WriteFileAtomic(outDir+string(os.PathSeparator)+name, b, 0o644); err != nil {
					return err
				}
			}
			// The plan tells the caller which directories to create and why.
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(struct {
				Directories any      `json:"directories"`
				Notes       []string `json:"notes"`
			}{files.Plan.Directories, files.Plan.Notes})
		},
	}
	f := c.Flags()
	f.StringVar(&settingsFile, "settings", "", "WorkerSettings protojson file")
	f.StringVar(&machineFile, "machine", "", "machine facts JSON file")
	f.StringVar(&outDir, "out", "", "output directory")
	for _, n := range []string{"settings", "machine", "out"} {
		_ = c.MarkFlagRequired(n)
	}
	return c
}

func selftestCmd(g *globalFlags, code *int) *cobra.Command {
	var out, buildUser string
	c := &cobra.Command{
		Use:   "selftest",
		Short: "Check IMDS, disks, time sync, FUSE/WinFSP and the Buildbarn binaries; write a JSON report",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := newEnv(g, cmd.ErrOrStderr(), "selftest")
			if err != nil {
				return err
			}
			defer e.close()
			ctx, cancel := signalContext()
			defer cancel()
			binDir := "/usr/local/bin"
			if runtime.GOOS == "windows" {
				binDir = `C:\bb\bin`
			}
			t := &workeragent.Selftest{Paths: e.paths, GOOS: runtime.GOOS, Version: version, IMDS: e.imds,
				Storage: storageFor(e), Exec: e.exec, FS: e.fs, BinDir: binDir, BuildUser: buildUser}
			report := t.Run(ctx)
			b, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			b = append(b, '\n')
			if out == "" || out == "-" {
				_, err = cmd.OutOrStdout().Write(b)
			} else {
				err = e.fs.WriteFileAtomic(out, b, 0o644)
			}
			if err != nil {
				return err
			}
			if !report.OK {
				*code = 1
				return errors.New("selftest failed")
			}
			return nil
		},
	}
	c.Flags().StringVar(&out, "out", "-", "write the JSON report here (- = stdout)")
	c.Flags().StringVar(&buildUser, "build-user", defaultBuildUser(), "check that this action user exists (empty skips)")
	return c
}
