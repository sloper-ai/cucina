// SPDX-License-Identifier: FSL-1.1-ALv2

// Command cucina-hostd is the macOS host agent of Cucina (R-MAC-1..6): a root
// LaunchDaemon (label ai.sloper.cucina.hostd) by default, or a developer
// user-mode process (--user-mode, T13). See docs/dev/hostd.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/sloper-ai/cucina/internal/hostd"
	"github.com/sloper-ai/cucina/internal/hostd/config"
	"github.com/sloper-ai/cucina/internal/hostd/facts"
	"github.com/sloper-ai/cucina/internal/hostd/identity"
	"github.com/sloper-ai/cucina/internal/hostd/logging"
	"github.com/sloper-ai/cucina/internal/hostd/privdrop"
	"github.com/sloper-ai/cucina/internal/hostd/render"
	"github.com/sloper-ai/cucina/internal/hostd/secretstore"
	"github.com/sloper-ai/cucina/internal/hostd/sys"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/providers/tart"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

// Exit codes (docs/dev/hostd.md §2).
const (
	exitOther     = 1
	exitConfig    = 2
	exitEnrollRef = 3
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func main() {
	root := &cobra.Command{Use: "cucina-hostd", Short: "Cucina macOS host agent", SilenceUsage: true}
	root.AddCommand(runCmd(), versionCmd(), factsCmd(), checkConfigCmd(), dropExecCmd())
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		os.Exit(exitOther)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print the version", Run: func(*cobra.Command, []string) {
		fmt.Println(version)
	}}
}

// dropExecCmd is the privilege-drop trampoline (ADR 0701); hidden.
func dropExecCmd() *cobra.Command {
	return &cobra.Command{
		Use: "drop-exec", Hidden: true, DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			t, err := privdrop.ParseTrampoline(args)
			if err != nil {
				return &exitError{code: 126, err: err}
			}
			err = privdrop.DropAndExec(t)
			fmt.Fprintln(os.Stderr, err)
			return &exitError{code: 126, err: err}
		},
	}
}

type flags struct {
	userMode       bool
	configPath     string
	stateDir       string
	logDir         string
	secretsDir     string
	tartPath       string
	bbStorage      string
	l2CacheDir     string
	privdrop       string
	storageRelay   string
	schedulerRelay string
	l2Listen       string
	l2Metrics      string
}

func (f *flags) bind(c *cobra.Command) {
	home, _ := os.UserHomeDir()
	fs := c.Flags()
	fs.BoolVar(&f.userMode, "user-mode", false, "run as the logged-in user (development, T13): no privilege drop, file secret store, user's tart")
	fs.StringVar(&f.configPath, "config", filepath.Join(home, ".config/cucina/hostd/config.plist"), "user mode: preferences plist (same keys as the managed preferences)")
	fs.StringVar(&f.stateDir, "state-dir", "", "state directory (default /var/db/cucina/hostd; user mode ~/.config/cucina/hostd/state)")
	fs.StringVar(&f.logDir, "log-dir", "", "log directory (default /Library/Logs/Cucina; user mode ~/Library/Logs/Cucina)")
	fs.StringVar(&f.secretsDir, "secrets-dir", filepath.Join(home, ".config/cucina/hostd/secrets"), "user mode: file secret store (0700)")
	fs.StringVar(&f.tartPath, "tart", "", "tart executable (default: TartPath preference; user mode: tart from PATH)")
	fs.StringVar(&f.bbStorage, "bb-storage", "", "bb_storage executable for the host L2 (default /usr/local/cucina/bin/bb_storage; user mode: disabled unless set)")
	fs.StringVar(&f.l2CacheDir, "l2-cache-dir", "", "host L2 cache data directory (default <state-dir>/l2/cache)")
	fs.StringVar(&f.privdrop, "privdrop", "", "privilege drop for tart: asuser (default as root) | setuid | none (user mode)")
	fs.StringVar(&f.storageRelay, "storage-relay", hostd.DefaultStorageRelay, "VM-facing relay to the host L2")
	fs.StringVar(&f.schedulerRelay, "scheduler-relay", hostd.DefaultSchedulerRelay, "VM-facing relay to the scheduler")
	fs.StringVar(&f.l2Listen, "l2-listen", hostd.DefaultL2Listen, "host L2 gRPC listener (loopback)")
	fs.StringVar(&f.l2Metrics, "l2-metrics", hostd.DefaultL2Metrics, "host L2 diagnostics listener (loopback)")
}

func loadConfig(f *flags) (config.Config, error) {
	if f.userMode {
		src, err := config.ReadPlistFile(f.configPath, false)
		if err != nil {
			return config.Config{}, err
		}
		return config.Load(src)
	}
	managed, err := config.ReadPlistFile(config.ManagedPlistPath, true)
	if err != nil {
		return config.Config{}, err
	}
	local, err := config.ReadPlistFile("/Library/Preferences/"+config.Domain+".plist", false)
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(config.NewCFSource(config.Domain, config.Layered{managed, local}))
}

func checkConfigCmd() *cobra.Command {
	f := &flags{}
	c := &cobra.Command{Use: "check-config", Short: "Validate the configuration (managed preferences) and exit",
		RunE: func(*cobra.Command, []string) error {
			cfg, err := loadConfig(f)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return &exitError{code: exitConfig, err: err}
			}
			fmt.Println(cfg.String())
			return nil
		}}
	f.bind(c)
	return c
}

func factsCmd() *cobra.Command {
	return &cobra.Command{Use: "facts", Short: "Print host facts as JSON (serial redacted)", RunE: func(c *cobra.Command, _ []string) error {
		g := facts.Gatherer{Exec: &sys.Exec{Mode: privdrop.ModeNone}, FS: sys.FS{}, DiskPath: "/", AgentVersion: version}
		fct, err := g.Gather(c.Context())
		fct.Serial = "<redacted>"
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(fct)
		return err
	}}
}

func runCmd() *cobra.Command {
	f := &flags{}
	c := &cobra.Command{Use: "run", Short: "Run the host agent", RunE: func(c *cobra.Command, _ []string) error {
		return run(f)
	}}
	f.bind(c)
	return c
}

func run(f *flags) error {
	home, _ := os.UserHomeDir()
	if !f.userMode && os.Geteuid() != 0 {
		return &exitError{code: exitConfig, err: errors.New("cucina-hostd must run as root (LaunchDaemon) or with --user-mode")}
	}
	if f.stateDir == "" {
		f.stateDir = "/var/db/cucina/hostd"
		if f.userMode {
			f.stateDir = filepath.Join(home, ".config/cucina/hostd/state")
		}
	}
	if f.logDir == "" {
		f.logDir = "/Library/Logs/Cucina"
		if f.userMode {
			f.logDir = filepath.Join(home, "Library/Logs/Cucina")
		}
	}
	level := new(slog.LevelVar)
	var extra io.Writer
	if f.userMode {
		extra = os.Stderr
	}
	log, closer, err := logging.Setup(f.logDir, level, extra)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()

	cfg, err := loadConfig(f)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		fmt.Fprintln(os.Stderr, err)
		return &exitError{code: exitConfig, err: err}
	}
	level.Set(logging.ParseLevel(cfg.LogLevel))
	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		return err
	}

	mode := privdrop.ModeAsUser
	if f.privdrop != "" {
		if mode, err = privdrop.ParseMode(f.privdrop); err != nil {
			return &exitError{code: exitConfig, err: err}
		}
	}
	tartBin := cfg.TartPath
	if f.tartPath != "" {
		tartBin = f.tartPath
	}
	var runAs *ports.RunAs
	var env []string
	var groups []uint32
	var secrets ports.SecretStore
	if f.userMode {
		mode = privdrop.ModeNone
		if f.tartPath == "" {
			if p, err := exec.LookPath("tart"); err == nil {
				tartBin = p
			}
		}
		env = os.Environ()
		secrets = secretstore.File{Dir: f.secretsDir}
	} else {
		u, err := user.Lookup(cfg.RunAsUser)
		if err != nil {
			err = fmt.Errorf("RunAsUser %q does not exist (the pkg's postinstall or MDM creates it): %w", cfg.RunAsUser, err)
			return &exitError{code: exitConfig, err: err}
		}
		uid, _ := strconv.ParseUint(u.Uid, 10, 32)
		gid, _ := strconv.ParseUint(u.Gid, 10, 32)
		gids, _ := u.GroupIds()
		for _, g := range gids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil && uint32(n) != uint32(gid) {
				groups = append(groups, uint32(n))
			}
		}
		pu := privdrop.User{Name: u.Username, UID: uint32(uid), GID: uint32(gid), Groups: groups, Home: u.HomeDir}
		runAs = pu.RunAs()
		env = privdrop.BaseEnv(pu)
		if !secretstore.KeychainAvailable {
			return &exitError{code: exitConfig, err: errors.New("this build has no keychain support (needs darwin+cgo)")}
		}
		secrets = secretstore.Keychain{Service: config.Domain}
	}
	self, _ := os.Executable()
	ex := &sys.Exec{Mode: mode, Self: self, Groups: groups}
	rt := tart.New(tart.Options{Exec: ex, Binary: tartBin, RunAs: runAs, Env: env, Logger: log.With("component", "tart")})

	bbStorage := f.bbStorage
	if bbStorage == "" && !f.userMode {
		bbStorage = "/usr/local/cucina/bin/bb_storage"
	}
	plainExec := &sys.Exec{Mode: privdrop.ModeNone}
	opts := hostd.Options{
		Config: cfg, UserMode: f.userMode, StateDir: f.stateDir, LogDir: f.logDir, Version: version,
		Exec: plainExec, FS: sys.FS{}, Clock: sys.Clock{}, Secrets: secrets, Runtime: rt, Render: render.BBConfig{},
		Facts: func(ctx context.Context) (facts.Facts, error) {
			return facts.Gatherer{Exec: plainExec, FS: sys.FS{}, DiskPath: f.stateDir, TartVersion: rt.Version, AgentVersion: version}.Gather(ctx)
		},
		L2Binary: bbStorage, L2CacheDir: f.l2CacheDir, L2Listen: f.l2Listen, L2Metrics: f.l2Metrics,
		StorageRelay: f.storageRelay, SchedulerRelay: f.schedulerRelay,
		LevelVar: level, Log: log,
	}
	if !f.userMode {
		opts.VMNet = hostd.DefaultsVMNet{Exec: plainExec}
	}
	a, err := hostd.New(opts)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err = a.Run(ctx)
	switch {
	case err == nil || (ctx.Err() != nil && errors.Is(err, context.Canceled)):
		log.Info("hostd stopped")
		return nil
	case errors.Is(err, identity.ErrDenied), errors.Is(err, identity.ErrTokenInvalid), errors.Is(err, identity.ErrNoToken), errors.Is(err, identity.ErrExpired):
		log.Error("enrollment refused", "err", err)
		return &exitError{code: exitEnrollRef, err: err}
	default:
		log.Error("hostd failed", "err", err)
		return &exitError{code: exitOther, err: err}
	}
}
