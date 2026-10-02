// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// transport executes one script and returns its (small) stdout.
type transport interface {
	exec(ctx context.Context, script string, timeout time.Duration) (stdout string, exit int, err error)
}

// scriptHost implements Host on top of a transport and a dialect.
type scriptHost struct {
	name, os, workDir string
	t                 transport
	d                 dialect
	// putChunk is the raw size of one inline upload chunk.
	putChunk int
	// bulk, if set, moves files larger than bulkThreshold (SSM port forwarding).
	bulk          bulkTransfer
	bulkThreshold int64
	jobsMu        sync.Mutex
	jobs          map[string]Job // launch authority survives transport calls, never reconstructed from PID files
}

type bulkTransfer interface {
	put(ctx context.Context, h *scriptHost, local, remote string) error
	get(ctx context.Context, h *scriptHost, remote, local string) error
}

func (h *scriptHost) Name() string    { return h.name }
func (h *scriptHost) OS() string      { return h.os }
func (h *scriptHost) WorkDir() string { return h.workDir }

func (h *scriptHost) jobDir(id string) string { return h.d.join(h.d.join(h.workDir, "jobs"), id) }

func (h *scriptHost) Run(ctx context.Context, script string, o Opts) (Result, error) {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	start := time.Now()
	dir := h.jobDir(newJobID())
	out, _, err := h.t.exec(ctx, h.d.foreground(dir, script, o), o.Timeout)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", h.name, err)
	}
	rc, outN, errN, stdout, stderr, err := parseForeground(out)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", h.name, err)
	}
	j := Job{ID: filepath.Base(dir), Dir: dir}
	if stdout, err = h.readRest(ctx, j, "stdout", stdout, outN); err != nil {
		return Result{}, err
	}
	if stderr, err = h.readRest(ctx, j, "stderr", stderr, errN); err != nil {
		return Result{}, err
	}
	return Result{ExitCode: rc, Stdout: stdout, Stderr: stderr, Duration: time.Since(start)}, nil
}

// readRest completes a stream whose head was inlined.
func (h *scriptHost) readRest(ctx context.Context, j Job, stream string, have []byte, size int64) ([]byte, error) {
	for int64(len(have)) < size {
		b, err := h.Read(ctx, j, stream, int64(len(have)), ChunkSize)
		if err != nil {
			return have, err
		}
		if len(b) == 0 {
			return have, fmt.Errorf("%s: %s: short read at %d/%d", h.name, stream, len(have), size)
		}
		have = append(have, b...)
	}
	return have, nil
}

// parseForeground decodes the marker line and the two inlined base64 heads.
func parseForeground(out string) (rc int, outN, errN int64, stdout, stderr []byte, err error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, resultMarker+" ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 4 {
			return 0, 0, 0, nil, nil, fmt.Errorf("malformed result line %q", line)
		}
		rc, _ = strconv.Atoi(f[1])
		outN, _ = strconv.ParseInt(f[2], 10, 64)
		errN, _ = strconv.ParseInt(f[3], 10, 64)
		var heads [2][]byte
		for i := range heads {
			if !sc.Scan() {
				return 0, 0, 0, nil, nil, fmt.Errorf("truncated output after result line")
			}
			heads[i], err = base64.StdEncoding.DecodeString(strings.TrimSpace(sc.Text()))
			if err != nil {
				return 0, 0, 0, nil, nil, fmt.Errorf("output head: %w", err)
			}
		}
		return rc, outN, errN, heads[0], heads[1], nil
	}
	return 0, 0, 0, nil, nil, fmt.Errorf("no %s line in transport output: %q", resultMarker, tail(out, 500))
}

func (h *scriptHost) Start(ctx context.Context, script string, o Opts) (Job, error) {
	id := newJobID()
	j := Job{ID: id, Dir: h.jobDir(id)}
	if t, ok := h.t.(jobTransport); ok {
		return t.startJob(ctx, j, script, o)
	}
	h.jobsMu.Lock()
	if h.jobs == nil {
		h.jobs = make(map[string]Job)
	}
	h.jobs[j.ID] = j
	h.jobsMu.Unlock()
	out, rc, err := h.t.exec(ctx, h.d.start(j.Dir, id, script, o), 2*time.Minute)
	if err != nil || rc != 0 || !strings.Contains(out, "started") {
		if err == nil {
			err = fmt.Errorf("exit %d: %s", rc, tail(out, 500))
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		return j, errors.Join(fmt.Errorf("%s: start job: %w", h.name, err), StopJob(cleanup, h, j))
	}
	return j, nil
}

func (h *scriptHost) Status(ctx context.Context, j Job) (JobStatus, error) {
	if t, ok := h.t.(jobTransport); ok {
		return t.jobStatus(j)
	}
	out, _, err := h.t.exec(ctx, h.d.status(j.Dir), time.Minute)
	if err != nil {
		return JobStatus{}, fmt.Errorf("%s: job status: %w", h.name, err)
	}
	return parseStatus(out)
}

func parseStatus(out string) (JobStatus, error) {
	f := strings.Fields(strings.TrimSpace(lastLine(out)))
	if len(f) != 4 {
		return JobStatus{}, fmt.Errorf("malformed job status %q", out)
	}
	st := JobStatus{State: f[0]}
	st.ExitCode, _ = strconv.Atoi(f[1])
	st.StdoutBytes, _ = strconv.ParseInt(f[2], 10, 64)
	st.StderrBytes, _ = strconv.ParseInt(f[3], 10, 64)
	switch st.State {
	case JobStarting, JobRunning, JobExited, JobLost:
		return st, nil
	}
	return st, fmt.Errorf("unknown job state %q", st.State)
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n ")
	if i := strings.LastIndexAny(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (h *scriptHost) Read(ctx context.Context, j Job, stream string, off int64, max int) ([]byte, error) {
	if stream != "stdout" && stream != "stderr" {
		return nil, fmt.Errorf("unknown stream %q", stream)
	}
	return h.readFile(ctx, h.d.join(j.Dir, stream), off, max)
}

func (h *scriptHost) readFile(ctx context.Context, file string, off int64, max int) ([]byte, error) {
	out, rc, err := h.t.exec(ctx, h.d.read(file, off, max), time.Minute)
	if err != nil {
		return nil, fmt.Errorf("%s: read %s: %w", h.name, file, err)
	}
	if rc != 0 {
		return nil, fmt.Errorf("%s: read %s: exit %d", h.name, file, rc)
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(lastLine(out)))
}

func (h *scriptHost) remoteSHA(ctx context.Context, file string) (string, error) {
	out, rc, err := h.t.exec(ctx, h.d.sha256(file), 5*time.Minute)
	if err != nil {
		return "", err
	}
	if rc != 0 {
		return "", fmt.Errorf("sha256 %s: exit %d", file, rc)
	}
	return strings.TrimSpace(lastLine(out)), nil
}

func (h *scriptHost) Put(ctx context.Context, local, remote string) error {
	fi, err := os.Stat(local)
	if err != nil {
		return err
	}
	want, err := fileSHA(local)
	if err != nil {
		return err
	}
	if h.bulk != nil && fi.Size() > h.bulkThreshold {
		if err := h.bulk.put(ctx, h, local, remote); err != nil {
			return err
		}
	} else if err := h.putInline(ctx, local, remote); err != nil {
		return err
	}
	got, err := h.remoteSHA(ctx, remote)
	if err != nil {
		return fmt.Errorf("%s: verify %s: %w", h.name, remote, err)
	}
	if got != want {
		return fmt.Errorf("%s: put %s: sha256 mismatch (local %s, remote %s)", h.name, remote, want, got)
	}
	return nil
}

func (h *scriptHost) putInline(ctx context.Context, local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, h.putChunk)
	first := true
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 || first {
			script := h.d.appendB64(remote, base64.StdEncoding.EncodeToString(buf[:n]), first)
			if _, rc, xerr := h.t.exec(ctx, script, 2*time.Minute); xerr != nil {
				return fmt.Errorf("%s: put %s: %w", h.name, remote, xerr)
			} else if rc != 0 {
				return fmt.Errorf("%s: put %s: exit %d", h.name, remote, rc)
			}
			first = false
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if _, rc, err := h.t.exec(ctx, h.d.commit(remote), 5*time.Minute); err != nil {
		return fmt.Errorf("%s: commit %s: %w", h.name, remote, err)
	} else if rc != 0 {
		return fmt.Errorf("%s: commit %s: exit %d", h.name, remote, rc)
	}
	return nil
}

func (h *scriptHost) Get(ctx context.Context, remote, local string) error {
	out, rc, err := h.t.exec(ctx, h.d.size(remote), time.Minute)
	if err != nil {
		return fmt.Errorf("%s: size %s: %w", h.name, remote, err)
	} else if rc != 0 {
		return fmt.Errorf("%s: size %s: exit %d", h.name, remote, rc)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(lastLine(out)), 10, 64)
	if err != nil {
		return fmt.Errorf("%s: size %s: %q", h.name, remote, out)
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	if h.bulk != nil && size > h.bulkThreshold {
		if err := h.bulk.get(ctx, h, remote, local); err != nil {
			return err
		}
	} else {
		var b bytes.Buffer
		for int64(b.Len()) < size {
			chunk, err := h.readFile(ctx, remote, int64(b.Len()), ChunkSize)
			if err != nil {
				return err
			}
			if len(chunk) == 0 {
				return fmt.Errorf("%s: get %s: short read at %d/%d", h.name, remote, b.Len(), size)
			}
			b.Write(chunk)
		}
		if err := os.WriteFile(local, b.Bytes(), 0o644); err != nil {
			return err
		}
	}
	want, err := h.remoteSHA(ctx, remote)
	if err != nil {
		return err
	}
	got, err := fileSHA(local)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: get %s: sha256 mismatch (remote %s, local %s)", h.name, remote, want, got)
	}
	return nil
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	s := sha256.New()
	if _, err := io.Copy(s, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(s.Sum(nil)), nil
}

// PutPrivate copies a secret file (a service-account key) to the host so
// that its content never appears in a transport command: SSM hosts use the
// port-forwarding path regardless of size (inline uploads would put the
// bytes into the Run Command parameters, which SSM keeps), local hosts copy
// directly. The file is made readable by its owner only (user on Linux;
// SYSTEM and Administrators on Windows).
func PutPrivate(ctx context.Context, h Host, local, remote, user string) error {
	sh, ok := h.(*scriptHost)
	if !ok {
		return fmt.Errorf("%s: private transfer unsupported", h.Name())
	}
	if _, isLocal := sh.t.(*localTransport); isLocal {
		b, err := os.ReadFile(local)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(remote), 0o700); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Dir(remote), 0o700); err != nil {
			return err
		}
		if fi, err := os.Lstat(remote); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing a private symlink destination")
		}
		f, err := os.OpenFile(remote, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return err
		}
		_, err = f.Write(b)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	if sh.bulk == nil {
		return fmt.Errorf("%s: refusing to send a secret inline; configure SSM port forwarding (profile/region)", h.Name())
	}
	// Restrict the staging destination BEFORE a receiver writes the first
	// secret byte. Final chmod alone leaves a window under the default umask.
	var prepare string
	if sh.os == Windows {
		prepare = fmt.Sprintf(`$ErrorActionPreference='Stop'
$p=%s; $d=Split-Path -Parent $p
New-Item -ItemType Directory -Force -Path $d | Out-Null
foreach ($item in @($d,$p,($p+'.part'))) { if ((Test-Path -LiteralPath $item) -and ((Get-Item -Force -LiteralPath $item).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'private destination is a reparse point' } }
$acl=New-Object System.Security.AccessControl.DirectorySecurity
$acl.SetAccessRuleProtection($true,$false)
foreach ($id in @('S-1-5-18','S-1-5-32-544')) {
  $sid=New-Object System.Security.Principal.SecurityIdentifier($id)
  $rule=New-Object System.Security.AccessControl.FileSystemAccessRule($sid,'FullControl','ContainerInherit,ObjectInherit','None','Allow')
  $acl.AddAccessRule($rule)
}
Set-Acl -LiteralPath $d -AclObject $acl
if (Test-Path ($p+'.part')) { Remove-Item -Force ($p+'.part') }
$fs=[IO.File]::Create($p+'.part'); $fs.Close()
`, psQuote(remote))
	} else {
		prepare = fmt.Sprintf(`set -eu
umask 077
p=%s; d=$(dirname "$p")
[ ! -L "$d" ] && [ ! -L "$p" ] && [ ! -L "$p.part" ] || exit 1
mkdir -p "$d"; chmod 0700 "$d"
: > "$p.part"; chmod 0600 "$p.part"
`, shQuote(remote))
	}
	if _, rc, err := sh.t.exec(ctx, prepare, time.Minute); err != nil {
		return fmt.Errorf("prepare private transfer: %w", err)
	} else if rc != 0 {
		return fmt.Errorf("prepare private transfer exited %d", rc)
	}
	if err := sh.bulk.put(ctx, sh, local, remote); err != nil {
		return err
	}
	var script string
	if sh.os == Windows {
		script = fmt.Sprintf("icacls %s /inheritance:r /grant:r 'SYSTEM:F' 'Administrators:F' | Out-Null", psQuote(remote))
	} else {
		script = fmt.Sprintf("chmod 0600 %s", shQuote(remote))
		if user != "" {
			script = fmt.Sprintf("chown %s %s && chown %s \"$(dirname %s)\" && %s", shQuote(user), shQuote(remote), shQuote(user), shQuote(remote), script)
		}
	}
	if _, rc, err := sh.t.exec(ctx, script, time.Minute); err != nil {
		return fmt.Errorf("%s: restrict %s: %w", h.Name(), remote, err)
	} else if rc != 0 {
		return fmt.Errorf("%s: restrict %s: exit %d", h.Name(), remote, rc)
	}
	return nil
}
