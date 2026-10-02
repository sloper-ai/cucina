// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/cenkalti/backoff/v7"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/bbconfig"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
	cucinaproto "github.com/sloper-ai/cucina/internal/proto"
	"github.com/sloper-ai/cucina/internal/workeragent/bootdata"
	"github.com/sloper-ai/cucina/internal/workeragent/imds"
)

// IMDS is the instance metadata the agent reads.
type IMDS interface {
	UserData(ctx context.Context) ([]byte, error)
	Identity(ctx context.Context) (raw []byte, doc imds.IdentityDocument, signature string, err error)
	Tags(ctx context.Context, prefix string) (map[string]string, error)
	SpotInstanceAction(ctx context.Context) (*imds.InstanceAction, error)
}

// DefaultBootstrapDeadline bounds transient retries (IMDS, enrollment) before
// the agent gives up and powers off (fail closed and fast).
const DefaultBootstrapDeadline = 2 * time.Minute

// minCertValidity is the minimum remaining certificate lifetime bootstrap
// accepts and supervise tolerates.
const minCertValidity = time.Hour

// Bootstrap is `cucina-worker-agent bootstrap`: enroll with the controller,
// write PKI files, prepare L1 and render the Buildbarn configuration
// (contracts §5.2). It never holds a long-lived secret: the key is generated
// at every boot and only its CSR leaves the process.
type Bootstrap struct {
	Paths   Paths
	GOOS    string // target operating system (runtime.GOOS)
	GOARCH  string // runtime.GOARCH
	Version string // agent version reported to the controller
	VCPUs   int

	IMDS IMDS
	// DialEnroller connects to the EnrollmentService named by the boot data.
	DialEnroller func(bootdata.BootData) (Enroller, func() error, error)
	Storage      Storage
	Renderer     Renderer
	Host         Host
	FS           ports.FS
	Clock        ports.Clock
	Power        Poweroff
	Log          *slog.Logger

	// BootDataFile, when set, replaces the IMDS user data.
	BootDataFile string
	// KeyReaders are extra Windows accounts (names or SIDs) granted read
	// access to the key file (a low-privilege bb_worker service account).
	KeyReaders []string
	// BuildUser is the unprivileged user actions run as (Linux runCommandsAs,
	// R-SEC-5); nil runs actions as bb_runner's user.
	BuildUser *UnixUser
	// Deadline bounds transient retries (default DefaultBootstrapDeadline).
	Deadline time.Duration
}

// BootError is a bootstrap failure; Reason is a stable machine-readable code
// (not-a-worker, boot-data, imds, host, enrollment-refused,
// enrollment-unreachable, enrollment-invalid, storage, render, write).
type BootError struct {
	Reason string
	Err    error
}

func (e *BootError) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *BootError) Unwrap() error { return e.Err }

func fail(reason string, err error) error { return &BootError{Reason: reason, Err: err} }

// ReasonNotAWorker marks an instance launched without any user data: not a
// controller launch (an image build, an EC2 Fast Launch pre-provisioning
// instance, a manual debug launch). Bootstrap fails without powering off so
// those pipelines keep working; the images' dead-man timer still bounds cost.
const ReasonNotAWorker = "not-a-worker"

// Run executes the bootstrap. On any failure except ReasonNotAWorker it powers
// the machine off (unless Power is a LogPoweroff) and returns the error: a
// worker that cannot enroll or configure itself is useless and must not linger
// (zero idle cost).
func (b *Bootstrap) Run(ctx context.Context) error {
	log := b.Log.With("cmd", "bootstrap")
	start := b.Clock.Now()
	deadline := b.Deadline
	if deadline <= 0 {
		deadline = DefaultBootstrapDeadline
	}
	err := b.run(ctx, log, start, start.Add(deadline))
	if err == nil {
		return nil
	}
	reason := "bootstrap"
	var be *BootError
	if errors.As(err, &be) {
		reason = be.Reason
	}
	if reason == ReasonNotAWorker {
		log.Warn("no boot data: not a Cucina worker launch; leaving the machine up", "event", "bootstrap.not_a_worker",
			"error", err.Error())
		return err
	}
	if ctx.Err() != nil { // stopped (service stop, OS shutdown, Ctrl-C): not a verdict on the machine
		log.Warn("bootstrap interrupted", "event", "bootstrap.interrupted", "error", err.Error())
		return err
	}
	log.Error("bootstrap failed; powering off", "event", "bootstrap.failed", "reason", reason, "error", err.Error(),
		"elapsed", b.Clock.Now().Sub(start).String())
	if perr := b.Power.PowerOff(context.WithoutCancel(ctx), "bootstrap: "+reason); perr != nil {
		log.Error("poweroff failed", "event", "poweroff.failed", "error", perr.Error())
	}
	return err
}

func (b *Bootstrap) run(ctx context.Context, log *slog.Logger, start, deadline time.Time) error {
	bootID, bootTime, err := b.bootIdentity()
	if err != nil {
		return fail("host", err)
	}
	if st, err := LoadState(b.FS, b.Paths.StateFile); err == nil && st.sameBoot(bootID, bootTime) {
		missing := b.missingOutputs(st)
		if missing == "" {
			log.Info("already bootstrapped during this boot", "event", "bootstrap.idempotent",
				"pool", st.Pool, "node", st.Node, "generation", st.Generation)
			return nil
		}
		log.Warn("bootstrapped earlier this boot but outputs are incomplete; bootstrapping again",
			"event", "bootstrap.repeat", "missing", missing)
	}
	log.Info("bootstrap starting", "event", "bootstrap.start", "version", b.Version, "os", b.GOOS, "arch", archName(b.GOARCH))

	bd, err := b.loadBootData(ctx, log, deadline)
	if err != nil {
		return err
	}
	log.Info("boot data loaded", "event", "bootdata.loaded", "enrollEndpoint", bd.EnrollEndpoint,
		"serverName", bd.TLSServerName(), "cluster", bd.Cluster, "pool", bd.Pool, "generation", bd.Generation)

	var (
		rawDoc []byte
		doc    imds.IdentityDocument
		sig    string
	)
	if err := b.retry(ctx, log, deadline, "imds.identity", func(error) bool { return false }, func(ctx context.Context) error {
		var err error
		rawDoc, doc, sig, err = b.IMDS.Identity(ctx)
		if err != nil && !imds.IsTransient(err) {
			return backoff.Permanent(err)
		}
		return err
	}); err != nil {
		return fail("imds", err)
	}
	log.Info("instance identity loaded", "event", "identity.loaded", "instanceId", doc.InstanceID,
		"instanceType", doc.InstanceType, "region", doc.Region, "availabilityZone", doc.AvailabilityZone)
	b.crossCheckTags(ctx, log, bd)

	key, reused, err := b.bootKey(bootID, bootTime)
	if err != nil {
		return fail("write", err)
	}
	if reused {
		log.Info("reusing the key of an earlier attempt in this boot", "event", "key.reused")
	}
	csr, err := key.csrPEM(doc.InstanceID)
	if err != nil {
		return fail("host", err)
	}
	enroller, closeEnroller, err := b.DialEnroller(bd)
	if err != nil {
		return fail("enrollment-refused", err)
	}
	defer func() { _ = closeEnroller() }()
	req := &cucinav1.EnrollWorkerRequest{
		Protocol:                 &cucinav1.ProtocolVersion{Major: cucinaproto.Major, Minor: cucinaproto.Minor},
		InstanceIdentityDocument: rawDoc,
		Signature:                sig,
		CsrPem:                   csr,
		AgentVersion:             b.Version,
		Os:                       b.GOOS,
		Arch:                     archName(b.GOARCH),
	}
	var resp *cucinav1.EnrollWorkerResponse
	err = b.retry(ctx, log, deadline, "enroll", enrollmentRefused, func(ctx context.Context) error {
		var err error
		resp, err = enroller.EnrollWorker(ctx, req)
		return err
	})
	switch {
	case err == nil:
	case enrollmentRefused(err):
		return fail("enrollment-refused", err)
	default:
		return fail("enrollment-unreachable", err)
	}

	now := b.Clock.Now()
	caPEM := resp.GetCaPem()
	if len(caPEM) == 0 {
		caPEM = []byte(bd.CAPEM)
	}
	id, err := validateIssued(resp.GetCertificatePem(), caPEM, key, now, minCertValidity)
	if err != nil {
		return fail("enrollment-invalid", err)
	}
	settings, err := b.checkSettings(resp, doc.InstanceID)
	if err != nil {
		return fail("enrollment-invalid", err)
	}
	log.Info("enrolled", "event", "enroll.ok", "pool", settings.GetPool(), "node", settings.GetNode(),
		"generation", resp.GetGeneration(), "certNotAfter", id.notAfter.UTC().Format(time.RFC3339),
		"schedulerEndpoint", settings.GetSchedulerEndpoint(), "storageEndpoint", settings.GetStorageEndpoint())

	if err := b.writePKI(id); err != nil {
		return fail("write", err)
	}

	m, err := b.prepareMachine(ctx, log, settings, id.caPEM)
	if err != nil {
		return err
	}
	files, err := RenderAll(b.Renderer, settings, m)
	if err != nil {
		return fail("render", err)
	}
	for _, n := range files.Plan.Notes {
		log.Info("configuration decision", "event", "config.note", "note", n)
	}
	if err := b.createDirectories(files.Plan, m); err != nil {
		return fail("write", err)
	}
	if err := b.writeConfigs(files); err != nil {
		return fail("write", err)
	}
	if err := b.writeDeadmanConfig(settings); err != nil {
		return fail("write", err)
	}
	log.Info("configuration rendered", "event", "config.rendered", "workerConfig", b.Paths.WorkerConfig(),
		"runnerConfig", b.Paths.RunnerConfig(), "envFile", b.Paths.EnvFile,
		"buildDirectory", files.Plan.BuildDirectory, "l1Placement", files.Plan.L1.Placement,
		"l1BlocksBytes", files.Plan.L1.BlocksBytes, "filePoolBytes", files.Plan.FilePoolBytes,
		"runnerThreads", files.Plan.TotalThreads)

	for _, p := range []string{b.Paths.LastActivityFile(), b.Paths.LastContactFile()} {
		if err := writeTimestamp(b.FS, p, now); err != nil {
			return fail("write", err)
		}
	}
	rawSettings, err := encodeSettings(settings)
	if err != nil {
		return fail("write", err)
	}
	st := &State{
		AgentVersion: b.Version, BootID: bootID, BootTime: bootTime, InstanceType: doc.InstanceType,
		Cluster: bd.Cluster, Pool: settings.GetPool(), Node: settings.GetNode(), Generation: resp.GetGeneration(),
		EnrolledAt: now, CertNotAfter: id.notAfter, Settings: rawSettings, Machine: m,
		L1Placement: files.Plan.L1.Placement,
	}
	if err := SaveState(b.FS, b.Paths.StateFile, st); err != nil {
		return fail("write", err)
	}
	log.Info("bootstrap complete", "event", "bootstrap.done", "pool", st.Pool, "node", st.Node,
		"generation", st.Generation, "elapsed", b.Clock.Now().Sub(start).String())
	return nil
}

func (b *Bootstrap) bootIdentity() (string, time.Time, error) {
	id, err := b.Host.BootID()
	if err != nil {
		return "", time.Time{}, err
	}
	up, err := b.Host.Uptime()
	if err != nil {
		return "", time.Time{}, err
	}
	return id, b.Clock.Now().Add(-up), nil
}

// missingOutputs returns the first output of a previous bootstrap that is
// missing or no longer usable ("" if all are present).
func (b *Bootstrap) missingOutputs(st *State) string {
	if st.CertNotAfter.Sub(b.Clock.Now()) < minCertValidity {
		return "certificate validity"
	}
	for _, p := range []string{b.Paths.KeyFile(), b.Paths.CertFile(), b.Paths.CAFile(), b.Paths.WorkerConfig(),
		b.Paths.RunnerConfig(), b.Paths.EnvFile, b.Paths.StateRoot, st.Machine.InstanceStorePath, st.Machine.DataVolumePath} {
		if p == "" {
			continue
		}
		if ok, err := b.FS.Exists(p); err != nil || !ok {
			return p
		}
	}
	return ""
}

func (b *Bootstrap) loadBootData(ctx context.Context, log *slog.Logger, deadline time.Time) (bootdata.BootData, error) {
	var raw []byte
	if b.BootDataFile != "" {
		var err error
		if raw, err = b.FS.ReadFile(b.BootDataFile); err != nil {
			return bootdata.BootData{}, fail("boot-data", err)
		}
	} else if err := b.retry(ctx, log, deadline, "imds.user-data", func(error) bool { return false }, func(ctx context.Context) error {
		var err error
		raw, err = b.IMDS.UserData(ctx)
		if errors.Is(err, imds.ErrNotFound) {
			return backoff.Permanent(fail(ReasonNotAWorker, errors.New("the instance has no user data (boot data)")))
		}
		if err != nil && !imds.IsTransient(err) {
			return backoff.Permanent(err)
		}
		return err
	}); err != nil {
		var be *BootError
		if errors.As(err, &be) {
			return bootdata.BootData{}, err
		}
		return bootdata.BootData{}, fail("imds", err)
	}
	bd, err := bootdata.Decode(raw)
	if err != nil {
		return bootdata.BootData{}, fail("boot-data", err)
	}
	return bd, nil
}

// crossCheckTags compares the launch tags with the boot data. A mismatch is
// logged (the controller verifies the tags itself; its response is
// authoritative).
func (b *Bootstrap) crossCheckTags(ctx context.Context, log *slog.Logger, bd bootdata.BootData) {
	tags, err := b.IMDS.Tags(ctx, "cucina:")
	if err != nil {
		log.Warn("instance tags unavailable through IMDS", "event", "tags.unavailable", "error", err.Error())
		return
	}
	for _, c := range []struct{ key, want string }{
		{domain.TagCluster, bd.Cluster}, {domain.TagPool, bd.Pool}, {domain.TagGeneration, bd.Generation},
	} {
		if got := tags[c.key]; got != "" && c.want != "" && got != c.want {
			log.Warn("launch tag disagrees with boot data", "event", "tags.mismatch", "tag", c.key, "tag_value", got, "bootdata_value", c.want)
		}
	}
}

func (b *Bootstrap) checkSettings(resp *cucinav1.EnrollWorkerResponse, instanceID string) (*cucinav1.WorkerSettings, error) {
	ws := resp.GetSettings()
	if ws == nil {
		return nil, errors.New("response carries no worker settings")
	}
	switch ws.GetNode() {
	case "":
		ws.Node = instanceID
	case instanceID:
	default:
		return nil, fmt.Errorf("settings are for node %q, this instance is %q", ws.GetNode(), instanceID)
	}
	if ws.GetPool() == "" {
		ws.Pool = resp.GetPool()
	}
	if ws.GetPool() == "" || (resp.GetPool() != "" && ws.GetPool() != resp.GetPool()) {
		return nil, fmt.Errorf("inconsistent pool (response %q, settings %q)", resp.GetPool(), ws.GetPool())
	}
	if ws.GetSchedulerEndpoint() == "" || ws.GetStorageEndpoint() == "" {
		return nil, errors.New("settings lack the scheduler or storage endpoint")
	}
	if len(ws.GetRunners()) == 0 {
		return nil, errors.New("settings declare no runner")
	}
	return ws, nil
}

// bootKey returns this boot's key: the one an earlier attempt of the same boot
// wrote (a bootstrap that crashed after enrolling re-enrolls with the same key,
// which the controller accepts within a short crash-recovery window), or a new
// one, written (0600) before it is used. Keys never survive a boot.
func (b *Bootstrap) bootKey(bootID string, bootTime time.Time) (*bootKey, bool, error) {
	marker := b.Paths.Style.Join(b.Paths.PKIDir, "worker.key.boot")
	if raw, err := b.FS.ReadFile(marker); err == nil {
		var m State
		if json.Unmarshal(raw, &m) == nil && m.sameBoot(bootID, bootTime) {
			if keyPEM, err := b.FS.ReadFile(b.Paths.KeyFile()); err == nil {
				if k, err := parseBootKey(keyPEM); err == nil {
					return k, true, nil
				}
			}
		}
	}
	k, err := newBootKey()
	if err != nil {
		return nil, false, err
	}
	keyPEM, err := k.pkcs8PEM()
	if err != nil {
		return nil, false, err
	}
	if err := b.FS.MkdirAll(b.Paths.PKIDir, 0o755); err != nil {
		return nil, false, err
	}
	if err := b.FS.WriteFileAtomic(b.Paths.KeyFile(), keyPEM, 0o600); err != nil {
		return nil, false, err
	}
	if err := protectKey(b.Paths.KeyFile(), b.KeyReaders); err != nil {
		return nil, false, err
	}
	raw, err := json.Marshal(State{BootID: bootID, BootTime: bootTime})
	if err != nil {
		return nil, false, err
	}
	return k, false, b.FS.WriteFileAtomic(marker, raw, 0o644)
}

// writePKI writes the certificate and CA bundle next to the key.
func (b *Bootstrap) writePKI(id *issued) error {
	if err := b.FS.WriteFileAtomic(b.Paths.CertFile(), id.certPEM, 0o644); err != nil {
		return err
	}
	return b.FS.WriteFileAtomic(b.Paths.CAFile(), id.caPEM, 0o644)
}

// prepareMachine resolves the L1 device, formats and mounts it, and collects
// the machine facts the renderer sizes L1 and the file pool from.
func (b *Bootstrap) prepareMachine(ctx context.Context, log *slog.Logger, ws *cucinav1.WorkerSettings, caPEM []byte) (Machine, error) {
	mem, err := b.Host.MemoryBytes()
	if err != nil {
		return Machine{}, fail("host", err)
	}
	disks, err := b.Storage.Disks(ctx)
	if err != nil {
		return Machine{}, fail("storage", err)
	}
	plan := PlanPlacement(ws.GetL1Placement(), disks)
	if plan.Fallback != "" {
		log.Warn("requested L1 placement unavailable; using fallback", "event", "l1.fallback",
			"requested", ws.GetL1Placement(), "placement", plan.Placement, "why", plan.Fallback)
	}
	// The renderer gets the resolved placement (an unavailable explicit request
	// must not make it fail).
	ws.L1Placement = plan.Placement
	vcpus := b.VCPUs
	if vcpus <= 0 {
		vcpus = runtime.NumCPU()
	}
	m := Machine{
		OS: b.GOOS, Arch: archName(b.GOARCH), VCPUs: vcpus, MemoryBytes: mem,
		StateRoot: b.Paths.StateRoot, BuildRoot: b.Paths.BuildRoot, RunDir: b.Paths.RunDir, PKIDir: b.Paths.PKIDir,
		CABundlePEM: string(caPEM), BuildUser: b.BuildUser,
	}
	device, mount := "", ""
	if plan.Disk != nil {
		device = plan.Disk.ID
		path, free, err := b.Storage.Prepare(ctx, plan, b.Paths.MountFor(plan.Placement))
		if err != nil {
			return Machine{}, fail("storage", err)
		}
		mount = path
		if plan.Placement == PlacementInstanceStore {
			m.InstanceStorePath, m.InstanceStoreBytes = path, free
		} else {
			m.DataVolumePath, m.DataVolumeBytes = path, free
		}
	}
	if err := b.FS.MkdirAll(b.Paths.StateRoot, 0o700); err != nil {
		return Machine{}, fail("write", err)
	}
	if _, free, err := b.FS.DiskUsage(b.Paths.StateRoot); err == nil {
		m.StateRootBytes = free
	}
	log.Info("L1 device resolved", "event", "l1.placed", "placement", plan.Placement, "device", device, "mount", mount,
		"instanceStoreBytes", m.InstanceStoreBytes, "dataVolumeBytes", m.DataVolumeBytes,
		"stateRootBytes", m.StateRootBytes, "vcpus", vcpus, "memoryBytes", mem)
	return m, nil
}

// createDirectories creates what bb_runner and bb_worker expect to exist
// (bbconfig plan) plus the agent's own directories.
func (b *Bootstrap) createDirectories(plan *bbconfig.WorkerPlan, m Machine) error {
	for _, d := range []string{b.Paths.BBDir, b.Paths.RunDir, b.Paths.Style.Dir(b.Paths.EnvFile), b.Paths.Style.Dir(b.Paths.StateFile)} {
		if err := b.FS.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for _, d := range plan.Directories {
		if err := b.FS.MkdirAll(d.Path, uint32(d.Mode.Perm())); err != nil {
			return err
		}
		if d.BuildUserOwned && m.BuildUser != nil {
			if err := chown(d.Path, int(m.BuildUser.UID), int(m.BuildUser.GID)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Bootstrap) writeConfigs(f RenderedFiles) error {
	if err := b.FS.WriteFileAtomic(b.Paths.WorkerConfig(), f.Worker, 0o644); err != nil {
		return err
	}
	if err := b.FS.WriteFileAtomic(b.Paths.RunnerConfig(), f.Runner, 0o644); err != nil {
		return err
	}
	return b.FS.WriteFileAtomic(b.Paths.EnvFile, f.Env, 0o644)
}

// writeDeadmanConfig gives the images' shell/PowerShell dead-man timers (the
// backstop when supervise is not running) the pool's limits, so both switches
// agree (R-POOL-7). Linux: /etc/cucina/deadman.env (sourced by
// cucina-deadman); Windows: C:\ProgramData\cucina\deadman.json.
func (b *Bootstrap) writeDeadmanConfig(ws *cucinav1.WorkerSettings) error {
	if b.Paths.DeadmanConfig == "" {
		return nil
	}
	l := LimitsFromSettings(ws.GetDeadman())
	secs := func(d time.Duration) int64 { return int64(d / time.Second) }
	var out []byte
	if b.Paths.Style == Windows {
		var err error
		out, err = json.Marshal(map[string]any{
			"enabled": true, "idle_limit_seconds": secs(l.Idle),
			"contact_limit_seconds": secs(l.Unreachable), "max_uptime_seconds": secs(l.MaxUptime),
		})
		if err != nil {
			return err
		}
	} else {
		out = fmt.Appendf(nil, "# Written by cucina-worker-agent bootstrap from the pool's dead-man settings.\n"+
			"ENABLED=1\nIDLE_LIMIT_SECONDS=%d\nCONTACT_LIMIT_SECONDS=%d\nMAX_UPTIME_SECONDS=%d\n",
			secs(l.Idle), secs(l.Unreachable), secs(l.MaxUptime))
	}
	return b.FS.WriteFileAtomic(b.Paths.DeadmanConfig, out, 0o644)
}

// retry runs f until it succeeds, fails permanently (refused(err) or
// backoff.Permanent), or the next jittered backoff would cross deadline.
func (b *Bootstrap) retry(ctx context.Context, log *slog.Logger, deadline time.Time, op string, refused func(error) bool, f func(context.Context) error) error {
	bo := &backoff.ExponentialBackOff{
		InitialInterval: time.Second, RandomizationFactor: 0.5, Multiplier: 2, MaxInterval: 15 * time.Second,
	}
	bo.Reset()
	for attempt := 1; ; attempt++ {
		per := 20 * time.Second
		if rem := deadline.Sub(b.Clock.Now()); rem < per {
			per = max(rem, time.Second)
		}
		actx, cancel := context.WithTimeout(ctx, per)
		err := f(actx)
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, backoff.ErrPermanent) {
			if inner := errors.Unwrap(err); inner != nil {
				return inner
			}
			return err
		}
		if refused(err) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := bo.NextBackOff()
		if b.Clock.Now().Add(wait).After(deadline) {
			return fmt.Errorf("%s: giving up after %d attempts: %w", op, attempt, err)
		}
		log.Warn("transient failure; retrying", "event", op+".retry", "attempt", attempt, "wait", wait.String(), "error", err.Error())
		if err := b.Clock.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}
