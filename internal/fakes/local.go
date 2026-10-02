// SPDX-License-Identifier: FSL-1.1-ALv2

package fakes

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/sloper-ai/cucina/internal/ports"
)

// ------------------------------------------------------------ SecretStore

// SecretStore is an in-memory keychain implementing ports.SecretStore.
type SecretStore struct {
	*Faults
	mu   sync.Mutex
	data map[string][]byte
}

var _ ports.SecretStore = (*SecretStore)(nil)

// NewSecretStore returns an empty store.
func NewSecretStore(clock ports.Clock, rnd *Rand) *SecretStore {
	return &SecretStore{Faults: newFaults(clock, rnd.Child("secretstore-faults")), data: map[string][]byte{}}
}

// Get implements ports.SecretStore.
func (s *SecretStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := s.enter(ctx, "Get"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	if !ok {
		return nil, fmt.Errorf("secret %q: %w", key, ports.ErrNotFound)
	}
	return slices.Clone(v), nil
}

// Put implements ports.SecretStore.
func (s *SecretStore) Put(ctx context.Context, key string, value []byte) error {
	if err := s.enter(ctx, "Put"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = slices.Clone(value)
	return nil
}

// Delete implements ports.SecretStore (deleting a missing key is not an error).
func (s *SecretStore) Delete(ctx context.Context, key string) error {
	if err := s.enter(ctx, "Delete"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

// ------------------------------------------------------------------- Exec

// ExecHandler answers one command.
type ExecHandler func(ctx context.Context, c ports.Command) (ports.ExecResult, error)

// Exec is a scripted process runner implementing ports.Exec. Commands are
// answered by the handler registered for their Path; unknown executables fail
// like a missing binary (fs.ErrNotExist).
type Exec struct {
	*Faults
	mu       sync.Mutex
	handlers map[string]ExecHandler
	log      []ports.Command
	pid      int
}

var _ ports.Exec = (*Exec)(nil)

// NewExec returns a runner with no executables.
func NewExec(clock ports.Clock, rnd *Rand) *Exec {
	return &Exec{Faults: newFaults(clock, rnd.Child("exec-faults")), handlers: map[string]ExecHandler{}, pid: 1000}
}

// Handle registers the handler for an executable path.
func (e *Exec) Handle(path string, h ExecHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[path] = h
}

// Commands returns the commands run so far (state for assertions on what was
// requested, e.g. the privilege drop of a tart invocation).
func (e *Exec) Commands() []ports.Command {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

func (e *Exec) handler(c ports.Command) (ExecHandler, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, c)
	h, ok := e.handlers[c.Path]
	if !ok {
		return nil, fmt.Errorf("exec %s: %w", c.Path, fs.ErrNotExist)
	}
	return h, nil
}

// Run implements ports.Exec.
func (e *Exec) Run(ctx context.Context, c ports.Command) (ports.ExecResult, error) {
	if err := e.enter(ctx, "Run"); err != nil {
		return ports.ExecResult{}, err
	}
	h, err := e.handler(c)
	if err != nil {
		return ports.ExecResult{}, err
	}
	return h(ctx, c)
}

// Start implements ports.Exec: the handler runs when the process is waited
// for; Signal("KILL"/"TERM"/"INT") ends it with exit code 128+n.
func (e *Exec) Start(ctx context.Context, c ports.Command) (ports.Process, error) {
	if err := e.enter(ctx, "Start"); err != nil {
		return nil, err
	}
	h, err := e.handler(c)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.pid++
	p := &process{pid: e.pid, h: h, c: c, done: make(chan struct{})}
	e.mu.Unlock()
	return p, nil
}

type process struct {
	mu     sync.Mutex
	pid    int
	h      ExecHandler
	c      ports.Command
	done   chan struct{}
	ended  bool
	result ports.ExecResult
	err    error
}

func (p *process) PID() int { return p.pid }

func (p *process) Wait(ctx context.Context) (ports.ExecResult, error) {
	p.mu.Lock()
	if !p.ended {
		p.result, p.err = p.h(ctx, p.c)
		p.ended = true
		close(p.done)
	}
	defer p.mu.Unlock()
	return p.result, p.err
}

func (p *process) Signal(sig string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return fmt.Errorf("process %d already exited: %w", p.pid, ports.ErrNotFound)
	}
	code := map[string]int{"INT": 130, "TERM": 143, "KILL": 137}[sig]
	if code == 0 {
		return fmt.Errorf("signal %q: %w", sig, ports.ErrInvalid)
	}
	p.result, p.ended = ports.ExecResult{ExitCode: code}, true
	close(p.done)
	return nil
}

// --------------------------------------------------------------------- FS

// FS is an in-memory filesystem implementing ports.FS with a capacity limit
// (writes beyond it fail with ports.ErrDiskFull). Errors wrap fs.ErrNotExist
// and fs.ErrExist like the os package.
type FS struct {
	*Faults
	mu       sync.Mutex
	files    map[string]memFile
	dirs     map[string]uint32
	capacity uint64
}

type memFile struct {
	data []byte
	mode uint32
}

var _ ports.FS = (*FS)(nil)

// NewFS returns an empty filesystem holding only "/" with the given capacity in bytes (0 = 1 TiB).
func NewFS(clock ports.Clock, rnd *Rand, capacity uint64) *FS {
	if capacity == 0 {
		capacity = 1 << 40
	}
	return &FS{Faults: newFaults(clock, rnd.Child("fs-faults")), files: map[string]memFile{}, dirs: map[string]uint32{"/": 0o755}, capacity: capacity}
}

// SetCapacity changes the volume size (e.g. to fill the disk).
func (f *FS) SetCapacity(bytes uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capacity = bytes
}

func clean(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func (f *FS) used() uint64 {
	var n uint64
	for _, fl := range f.files {
		n += uint64(len(fl.data))
	}
	return n
}

// ReadFile implements ports.FS.
func (f *FS) ReadFile(p string) ([]byte, error) {
	if err := f.enter(context.Background(), "ReadFile"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fl, ok := f.files[clean(p)]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return slices.Clone(fl.data), nil
}

// WriteFileAtomic implements ports.FS: the parent directory must exist; the
// file is replaced in one step.
func (f *FS) WriteFileAtomic(p string, data []byte, mode uint32) error {
	if err := f.enter(context.Background(), "WriteFileAtomic"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	if _, ok := f.dirs[path.Dir(p)]; !ok {
		return &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	if _, ok := f.dirs[p]; ok {
		return &fs.PathError{Op: "open", Path: p, Err: fs.ErrExist}
	}
	old := uint64(len(f.files[p].data))
	if f.used()-old+uint64(len(data)) > f.capacity {
		return &fs.PathError{Op: "write", Path: p, Err: ports.ErrDiskFull}
	}
	f.files[p] = memFile{data: slices.Clone(data), mode: mode}
	return nil
}

// MkdirAll implements ports.FS.
func (f *FS) MkdirAll(p string, mode uint32) error {
	if err := f.enter(context.Background(), "MkdirAll"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	for q := p; ; q = path.Dir(q) {
		if _, ok := f.files[q]; ok {
			return &fs.PathError{Op: "mkdir", Path: q, Err: fs.ErrExist}
		}
		if q == "/" {
			break
		}
	}
	for q := p; q != "/"; q = path.Dir(q) {
		if _, ok := f.dirs[q]; !ok {
			f.dirs[q] = mode
		}
	}
	return nil
}

// Remove implements ports.FS (a non-empty directory is an error).
func (f *FS) Remove(p string) error {
	if err := f.enter(context.Background(), "Remove"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	if _, ok := f.files[p]; ok {
		delete(f.files, p)
		return nil
	}
	if _, ok := f.dirs[p]; !ok || p == "/" {
		return &fs.PathError{Op: "remove", Path: p, Err: fs.ErrNotExist}
	}
	if len(f.children(p)) > 0 {
		return &fs.PathError{Op: "remove", Path: p, Err: fs.ErrExist}
	}
	delete(f.dirs, p)
	return nil
}

// RemoveAll implements ports.FS (missing paths are not an error).
func (f *FS) RemoveAll(p string) error {
	if err := f.enter(context.Background(), "RemoveAll"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	pre := strings.TrimSuffix(p, "/") + "/"
	for k := range f.files {
		if k == p || strings.HasPrefix(k, pre) {
			delete(f.files, k)
		}
	}
	for k := range f.dirs {
		if k != "/" && (k == p || strings.HasPrefix(k, pre)) {
			delete(f.dirs, k)
		}
	}
	return nil
}

// Rename implements ports.FS for files and directories.
func (f *FS) Rename(oldpath, newpath string) error {
	if err := f.enter(context.Background(), "Rename"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, n := clean(oldpath), clean(newpath)
	if _, ok := f.dirs[path.Dir(n)]; !ok {
		return &fs.PathError{Op: "rename", Path: n, Err: fs.ErrNotExist}
	}
	if fl, ok := f.files[o]; ok {
		delete(f.files, o)
		f.files[n] = fl
		return nil
	}
	if _, ok := f.dirs[o]; !ok || o == "/" {
		return &fs.PathError{Op: "rename", Path: o, Err: fs.ErrNotExist}
	}
	pre := o + "/"
	for k, v := range f.files {
		if strings.HasPrefix(k, pre) {
			delete(f.files, k)
			f.files[n+"/"+strings.TrimPrefix(k, pre)] = v
		}
	}
	for k, v := range f.dirs {
		if k == o || strings.HasPrefix(k, pre) {
			delete(f.dirs, k)
			f.dirs[n+strings.TrimPrefix(k, o)] = v
		}
	}
	return nil
}

// Exists implements ports.FS.
func (f *FS) Exists(p string) (bool, error) {
	if err := f.enter(context.Background(), "Exists"); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	_, isFile := f.files[p]
	_, isDir := f.dirs[p]
	return isFile || isDir, nil
}

// DiskUsage implements ports.FS.
func (f *FS) DiskUsage(p string) (used, free uint64, err error) {
	if err := f.enter(context.Background(), "DiskUsage"); err != nil {
		return 0, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	used = f.used()
	if used > f.capacity {
		return used, 0, nil
	}
	return used, f.capacity - used, nil
}

// ListDir implements ports.FS (sorted entry names).
func (f *FS) ListDir(p string) ([]string, error) {
	if err := f.enter(context.Background(), "ListDir"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p = clean(p)
	if _, ok := f.dirs[p]; !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return f.children(p), nil
}

func (f *FS) children(dir string) []string {
	set := map[string]bool{}
	pre := strings.TrimSuffix(dir, "/") + "/"
	for _, m := range []map[string]bool{keysOf(f.files), keysOfDirs(f.dirs)} {
		for k := range m {
			if k != dir && strings.HasPrefix(k, pre) && !strings.Contains(strings.TrimPrefix(k, pre), "/") {
				set[strings.TrimPrefix(k, pre)] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysOf(m map[string]memFile) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func keysOfDirs(m map[string]uint32) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
