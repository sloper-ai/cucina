// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build darwin || linux

package remote

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The anchor catches TERM (not SIG_IGN, which a payload would inherit), stays
// alive after payload exit, and releases only on its owner's private pipe. A
// stopped wait never reports normal completion. Payloads inherit neither FD.
const processAnchor = `stopping=0
complete=0
trap 'stopping=1' TERM INT HUP
printf 'ready\n' >&4
"$@" 3<&- 4>&- &
child=$!
exec >/dev/null 2>&1
wait "$child"
rc=$?
if [ "$stopping" = 0 ]; then complete=1; printf 'exit %s\n' "$rc" >&4; fi
while :; do
  command=
  IFS= read -r command <&3
  read_rc=$?
  if [ "$read_rc" = 0 ] && [ "$command" = release ] && [ "$complete" = 1 ] && [ "$stopping" = 0 ]; then exit "$rc"; fi
  if [ "$read_rc" -gt 128 ] && [ "$stopping" = 1 ]; then continue; fi
  # Owner disappeared or sent an invalid control message. This still-live
  # anchor can safely terminate its own group without any stale numeric lookup.
  kill -TERM 0
  sleep 0.25
  kill -KILL 0
  exit 127
done
`

type forwardProcess struct {
	cmd                                                *exec.Cmd
	group                                              int
	notice                                             *processNotice
	controlRead, controlWrite, statusRead, statusWrite *os.File
	status                                             *bufio.Reader
}

func ownForwardProcess(cmd *exec.Cmd) (*forwardProcess, error) {
	if len(cmd.ExtraFiles) != 0 {
		return nil, errors.New("owned command already uses reserved control descriptors")
	}
	notice, err := newProcessNotice()
	if err != nil {
		return nil, err
	}
	p := &forwardProcess{cmd: cmd, notice: notice}
	if p.controlRead, p.controlWrite, err = os.Pipe(); err != nil {
		_ = notice.close()
		return nil, err
	}
	if p.statusRead, p.statusWrite, err = os.Pipe(); err != nil {
		_ = p.close()
		return nil, err
	}
	p.status = bufio.NewReaderSize(p.statusRead, 64)
	original := append([]string{cmd.Path}, cmd.Args[1:]...)
	cmd.Path = "/bin/sh"
	cmd.Args = append([]string{"/bin/sh", "-c", processAnchor, "cucina-owned-command"}, original...)
	cmd.ExtraFiles = []*os.File{p.controlRead, p.statusWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return p, nil
}
func (p *forwardProcess) started() error {
	p.group = p.cmd.Process.Pid
	_ = p.controlRead.Close()
	p.controlRead = nil
	_ = p.statusWrite.Close()
	p.statusWrite = nil
	if p.group <= 1 {
		return errors.New("invalid owned process group")
	}
	if err := p.notice.bind(p.group); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	line, err := p.readStatus(ctx)
	if err != nil {
		return err
	}
	if line != "ready" {
		return errors.New("owned process anchor did not become ready")
	}
	return nil
}
func (p *forwardProcess) readStatus(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := p.statusRead.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return "", err
		}
		line, err := p.status.ReadSlice('\n')
		if errors.Is(err, os.ErrDeadlineExceeded) && len(line) == 0 {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("owned process anchor status: %w", err)
		}
		return strings.TrimSuffix(string(line), "\n"), nil
	}
}
func (p *forwardProcess) waitExited(ctx context.Context) error {
	line, err := p.readStatus(ctx)
	if err != nil {
		return err
	}
	prefix, value, ok := strings.Cut(line, " ")
	rc, parseErr := strconv.Atoi(value)
	if !ok || prefix != "exit" || parseErr != nil || rc < 0 || rc > 255 {
		return errors.New("invalid owned payload completion record")
	}
	return nil
}
func (p *forwardProcess) release() error {
	if err := p.controlWrite.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err := p.controlWrite.Write([]byte("release\n"))
	return err
}
func (p *forwardProcess) waitReap(ctx context.Context) error { return p.notice.wait(ctx, p.group) }
func (p *forwardProcess) close() error {
	var errs []error
	for _, f := range []*os.File{p.controlRead, p.controlWrite, p.statusRead, p.statusWrite} {
		if f != nil {
			errs = append(errs, f.Close())
		}
	}
	return errors.Join(append(errs, p.notice.close())...)
}
func (p *forwardProcess) terminate() error { return p.signal(syscall.SIGTERM) }
func (p *forwardProcess) kill() error      { return p.signal(syscall.SIGKILL) }
func (p *forwardProcess) signal(s syscall.Signal) error {
	if p.group <= 1 {
		return errors.New("refusing to signal an unreserved process group")
	}
	err := syscall.Kill(-p.group, s)
	if err != nil {
		return fmt.Errorf("signal owned process group with %s: %w", s, err)
	}
	return nil
}
