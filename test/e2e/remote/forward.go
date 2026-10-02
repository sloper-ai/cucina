// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// portForward moves large files (cucinactl binaries, BEP/exec-log/profile
// artifacts) without S3 and without inbound ports: a one-shot HTTP receiver
// bound to 127.0.0.1 on the VM (python3 on Linux, HttpListener on Windows) is
// reached from the dev Mac through an SSM port-forwarding session
// (AWS-StartPortForwardingSession). The URL path carries a random token; the
// end-to-end SHA-256 is verified by Put/Get.
type portForward struct {
	instanceID, profile, region string
}

const pyReceiver = `import http.server, os, shutil, sys, time
path, port, token, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
done = []
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def ok(self):
        if self.path != "/" + token:
            self.send_error(403); return False
        return True
    def do_PUT(self):
        if mode != "put" or not self.ok(): return
        n = int(self.headers["Content-Length"])
        os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
        with open(path + ".part", "wb") as f:
            while n > 0:
                b = self.rfile.read(min(n, 1 << 20))
                if not b: break
                f.write(b); n -= len(b)
        os.replace(path + ".part", path)
        self.send_response(204); self.end_headers(); done.append(1)
    def do_GET(self):
        if mode != "get" or not self.ok(): return
        self.send_response(200)
        self.send_header("Content-Length", str(os.path.getsize(path))); self.end_headers()
        with open(path, "rb") as f: shutil.copyfileobj(f, self.wfile, 1 << 20)
        done.append(1)
s = http.server.HTTPServer(("127.0.0.1", port), H); s.timeout = 5
deadline = time.time() + 900
while not done and time.time() < deadline: s.handle_request()
`

const psReceiver = `param([string]$Path, [int]$Port, [string]$Token, [string]$Mode)
$l = New-Object System.Net.HttpListener; $l.Prefixes.Add("http://127.0.0.1:$Port/"); $l.Start()
$deadline = (Get-Date).AddMinutes(15); $done = $false
try {
  while (-not $done -and (Get-Date) -lt $deadline) {
    $t = $l.GetContextAsync(); while (-not $t.Wait(5000)) { if ((Get-Date) -gt $deadline) { return } }
    $c = $t.Result
    if ($c.Request.Url.AbsolutePath -ne "/$Token") { $c.Response.StatusCode = 403; $c.Response.Close(); continue }
    if ($Mode -eq 'put' -and $c.Request.HttpMethod -eq 'PUT') {
      New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Path) | Out-Null
      $fs = [IO.File]::Create("$Path.part"); $c.Request.InputStream.CopyTo($fs); $fs.Close()
      Move-Item -Force "$Path.part" $Path; $c.Response.StatusCode = 204; $done = $true
    } elseif ($Mode -eq 'get' -and $c.Request.HttpMethod -eq 'GET') {
      $fs = [IO.File]::OpenRead($Path); $c.Response.ContentLength64 = $fs.Length; $fs.CopyTo($c.Response.OutputStream); $fs.Close(); $done = $true
    } else { $c.Response.StatusCode = 405 }
    $c.Response.Close()
  }
} finally { $l.Stop() }
`

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// receiver starts the one-shot receiver job on the VM.
func (p *portForward) receiver(ctx context.Context, h *scriptHost, path, mode, token string, port int) (Job, error) {
	var script string
	if h.os == Windows {
		script = fmt.Sprintf("$s = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String(%s))\n& ([ScriptBlock]::Create($s)) -Path %s -Port %d -Token %s -Mode %s\n",
			psQuote(b64(psReceiver)), psQuote(path), port, psQuote(token), mode)
	} else {
		script = fmt.Sprintf("exec python3 -c \"$(printf '%%s' %s | base64 -d)\" %s %d %s %s\n",
			shQuote(b64(pyReceiver)), shQuote(path), port, shQuote(token), mode)
	}
	return h.Start(ctx, script, Opts{})
}

// session opens `aws ssm start-session` port forwarding and waits until the
// plugin reports it is listening.
func (p *portForward) session(ctx context.Context, remotePort int) (int, func(), error) {
	lport, err := freeLocalPort()
	if err != nil {
		return 0, nil, err
	}
	params := fmt.Sprintf(`{"portNumber":["%d"],"localPortNumber":["%d"]}`, remotePort, lport)
	sctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(sctx, "aws", "ssm", "start-session", "--target", p.instanceID,
		"--document-name", "AWS-StartPortForwardingSession", "--parameters", params,
		"--profile", p.profile, "--region", p.region)
	cmd.Env = append(os.Environ(), "AWS_PAGER=")
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		cancel()
		return 0, nil, fmt.Errorf("aws ssm start-session: %w", err)
	}
	ready := make(chan struct{})
	var once sync.Once
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "Waiting for connections") {
				once.Do(func() { close(ready) })
			}
		}
	}()
	stop := func() {
		cancel()
		_ = cmd.Wait()
		_ = pw.Close()
	}
	select {
	case <-ready:
		return lport, stop, nil
	case <-time.After(90 * time.Second):
		stop()
		return 0, nil, fmt.Errorf("ssm port forwarding to %s: not ready after 90s", p.instanceID)
	case <-ctx.Done():
		stop()
		return 0, nil, ctx.Err()
	}
}

func (p *portForward) transfer(ctx context.Context, h *scriptHost, remote, mode string, do func(url string) error) error {
	token := randomHex(16)
	port := 20000 + int(time.Now().UnixNano()%20000)
	j, err := p.receiver(ctx, h, remote, mode, token, port)
	if err != nil {
		return err
	}
	lport, stop, err := p.session(ctx, port)
	if err != nil {
		return err
	}
	defer stop()
	url := fmt.Sprintf("http://127.0.0.1:%d/%s", lport, token)
	var last error
	for attempt := 0; attempt < 10; attempt++ { // the receiver may still be starting
		if last = do(url); last == nil {
			break
		}
		if err := RealSleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	if last != nil {
		return fmt.Errorf("%s: %s %s via port forwarding: %w", h.name, mode, remote, last)
	}
	_, err = Wait(ctx, h, j, 2*time.Second, nil)
	return err
}

func (p *portForward) put(ctx context.Context, h *scriptHost, local, remote string) error {
	return p.transfer(ctx, h, remote, "put", func(url string) error {
		f, err := os.Open(local)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		fi, _ := f.Stat()
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
		if err != nil {
			return err
		}
		req.ContentLength = fi.Size()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("receiver: %s", resp.Status)
		}
		return nil
	})
}

func (p *portForward) get(ctx context.Context, h *scriptHost, remote, local string) error {
	return p.transfer(ctx, h, remote, "get", func(url string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("receiver: %s", resp.Status)
		}
		f, err := os.Create(local)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	})
}
