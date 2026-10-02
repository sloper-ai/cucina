// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/collect/execlog"
	"github.com/sloper-ai/cucina/test/e2e/collect/grpcuploads"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// prepareUploadProvenance resolves rules on the ACTUAL client before any
// remote build. It uses no remote rc/cache/executor, so this is not a seed
// upload. Raw definitions can contain request-like data and remain private.
func (lr *laneRun) prepareUploadProvenance() error {
	lr.inventoryOnce.Do(func() {
		c, h := lr.c, lr.host
		path, err := privateRPCPath(c, lr, "resolved-repositories-"+c.Scenario.ID)
		if err != nil {
			lr.inventoryErr = err
			return
		}
		bz := bazelBinary(c.Env, h)
		if bz == "" {
			bz = "bazel"
			if h.OS() == remote.Windows {
				bz = "bazel.exe"
			}
		}
		command := quoteFor(h, bz) + " --nohome_rc --nosystem_rc mod show_repo --all_repos --remote_cache= --remote_executor="
		var script string
		if h.OS() == remote.Windows {
			script = "$ErrorActionPreference='Stop'; Set-Location " + argFor(h, lr.ws) + "; " + command + " | Out-File -Encoding utf8 " + argFor(h, path) + "; exit $LASTEXITCODE"
		} else {
			script = "set -eu; umask 077; cd " + argFor(h, lr.ws) + "; " + command + " > " + argFor(h, path)
		}
		_, r, err := remote.RunJob(c, h, script, remote.Opts{User: lr.user}, 5*time.Second, nil)
		if err == nil {
			err = r.Err()
		}
		if err != nil {
			lr.inventoryErr = fmt.Errorf("resolve actual client repository definitions: %w", err)
			return
		}
		home, err := os.UserHomeDir()
		if err != nil {
			lr.inventoryErr = err
			return
		}
		dir := filepath.Join(home, ".config", "cucina", "e2e", c.Env.RunID, "repo-provenance")
		if err = os.MkdirAll(dir, 0o700); err != nil {
			lr.inventoryErr = err
			return
		}
		if err = os.Chmod(dir, 0o700); err != nil {
			lr.inventoryErr = err
			return
		}
		local := filepath.Join(dir, safeName(c.Scenario.ID+"-"+h.Name())+".txt")
		f, err := os.OpenFile(local, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			lr.inventoryErr = err
			return
		}
		_ = f.Close()
		if err = h.Get(c, path, local); err != nil {
			lr.inventoryErr = err
			return
		}
		if err = os.Chmod(local, 0o600); err != nil {
			lr.inventoryErr = err
			return
		}
		b, err := os.ReadFile(local)
		if err != nil {
			lr.inventoryErr = err
			return
		}
		text := strings.TrimPrefix(string(b), "\ufeff")
		lr.actualInventory, lr.inventoryErr = grpcuploads.InventoryFromRules(text)
		sum := sha256.Sum256(b)
		lr.inventoryProof = hex.EncodeToString(sum[:])
		if lr.inventoryErr == nil {
			c.Record("repositoryProvenance."+h.Name(), map[string]string{"sha256": lr.inventoryProof, "client": h.Name(), "command": "bazel --nohome_rc --nosystem_rc mod show_repo --all_repos --remote_cache= --remote_executor="})
		}
	})
	return lr.inventoryErr
}

func uploadInventory(c *harness.Context) (grpcuploads.Inventory, error) {
	if c.Env.CrossInventoryFile == "" {
		return grpcuploads.Inventory{}, harness.Skip("NFR-X5 needs crossInventoryFile: explicit canonical repository/component/version/variant/immutable-pin map from the resolved toolchain definitions")
	}
	b, err := os.ReadFile(c.Env.CrossInventoryFile)
	if err != nil {
		return grpcuploads.Inventory{}, err
	}
	return grpcuploads.ParseInventory(b)
}

// privateRPCPath never places raw RPC logs in the bulk artifact directory.
// The log contains RequestMetadata and may contain ActionResult/request data;
// absence of Authorization logging is not a blanket secret-free guarantee.
func privateRPCPath(c *harness.Context, lr *laneRun, name string) (string, error) {
	h := lr.host
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid RPC log name")
	}
	if h.Name() == infra.DevMac {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir := filepath.Join(home, ".config", "cucina", "e2e", c.Env.RunID, "rpc")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
		return filepath.Join(dir, name+".pb"), nil
	}
	var script string
	if h.OS() == remote.Windows {
		rel := ".config/cucina/e2e/" + c.Env.RunID + "/rpc"
		script = fmt.Sprintf("$ErrorActionPreference='Stop'; $d=Join-Path $env:USERPROFILE %s; New-Item -ItemType Directory -Force $d | Out-Null; & icacls.exe $d /inheritance:r /grant:r 'SYSTEM:(OI)(CI)F' 'Administrators:(OI)(CI)F' | Out-Null; if ($LASTEXITCODE) { exit $LASTEXITCODE }; (Join-Path $d %s).Replace('\\','/')", argFor(h, rel), argFor(h, name+".pb"))
	} else {
		rel := ".config/cucina/e2e/" + c.Env.RunID + "/rpc"
		script = "set -eu; umask 077; d=\"$HOME/" + rel + "\"; mkdir -p \"$d\"; chmod 700 \"$d\"; printf '%s/%s\\n' \"$d\" " + argFor(h, name+".pb")
	}
	r, err := h.Run(c, script, remote.Opts{User: lr.user})
	if err != nil {
		return "", err
	}
	if err = r.Err(); err != nil {
		return "", err
	}
	path := strings.TrimSpace(lastLine(string(r.Stdout)))
	if path == "" || strings.ContainsAny(path, "\r\n") {
		return "", fmt.Errorf("private RPC log directory could not be established")
	}
	return path, nil
}

// collectUploadTrace transfers raw bytes only into the encrypted private
// config tree, then exports an allow-listed sanitized capture to bulk storage.
// Raw logs are never c.Artifact attachments and no error includes raw content.
func collectUploadTrace(c *harness.Context, lr *laneRun, cfg Config, step, remotePath string, o *bazelrun.Outcome, inventory grpcuploads.Inventory) (string, error) {
	if o == nil || o.BEP == nil || o.ExecLogPath == "" {
		return "", fmt.Errorf("X5 lacks BEP/execution-log invocation evidence")
	}
	if !o.Succeeded() || !o.BEP.LastMessage {
		return "", fmt.Errorf("X5 invocation did not complete successfully")
	}
	l, err := execlog.ReadFile(o.ExecLogPath)
	if err != nil {
		return "", err
	}
	if l.InvocationID == "" || l.InvocationID != o.BEP.UUID {
		return "", fmt.Errorf("BEP/execution-log invocation identity mismatch")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	privateDir := filepath.Join(home, ".config", "cucina", "e2e", c.Env.RunID, "rpc-collected")
	if err := os.MkdirAll(privateDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(privateDir, 0o700); err != nil {
		return "", err
	}
	base := safeName(cfg.Name) + "-" + step
	f, err := os.CreateTemp(privateDir, base+"-*.pb")
	if err != nil {
		return "", err
	}
	local := f.Name()
	if err = f.Close(); err != nil {
		return "", err
	}
	if err := lr.host.Get(c, remotePath, local); err != nil {
		return "", err
	}
	if err := os.Chmod(local, 0o600); err != nil {
		return "", err
	}
	f, err = os.Open(local)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	scope := sha256.Sum256([]byte(c.Env.Endpoints.RemoteExecution + "\x00" + c.Env.Endpoints.InstanceName))
	trace, err := grpcuploads.Read(f, grpcuploads.Options{Config: cfg.Name, InvocationID: l.InvocationID, Scope: hex.EncodeToString(scope[:]), Instance: c.Env.Endpoints.InstanceName, HashFunction: l.HashFunction, Inventory: inventory})
	if err != nil {
		return "", fmt.Errorf("RPC log sanitizer refused input: %w", err)
	}
	capture := grpcuploads.Capture{Trace: trace}
	known := map[string]bool{}
	for _, r := range inventory.Repositories {
		known[r.Name] = true
	}
	seen := map[string]bool{}
	for _, in := range l.Inputs {
		if in.Digest.IsZero() {
			continue
		}
		p := strings.ReplaceAll(in.Path, `\`, "/")
		var rest string
		if _, r, ok := strings.Cut(p, "external/"); ok {
			rest = r
		} else if strings.HasPrefix(p, "../") {
			rest = strings.TrimPrefix(p, "../")
		}
		repo, _, _ := strings.Cut(rest, "/")
		if !known[repo] {
			if in.Tool && (repo == "llvm+" || strings.HasPrefix(repo, "llvm++") || strings.HasPrefix(repo, "windows_support++") || strings.HasPrefix(repo, "cucina_platforms++apple+")) {
				capture.Trace.Issues = append(capture.Trace.Issues, "used external tool repository has no explicit inventory pin")
			}
			continue
		}
		key := repo + "\x00" + in.Digest.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		capture.Files = append(capture.Files, grpcuploads.File{Repository: repo, Digest: grpcuploads.Digest{Hash: in.Digest.Hash, Size: in.Digest.SizeBytes}})
	}
	var exercised []string
	for _, m := range capture.Trace.Manifests {
		exercised = append(exercised, m.Repository)
	}
	for _, file := range capture.Files {
		exercised = append(exercised, file.Repository)
	}
	if err := grpcuploads.VerifyRepositoryPins(inventory, lr.actualInventory, exercised); err != nil {
		return "", err
	}
	if lr.inventoryProof == "" {
		return "", fmt.Errorf("missing actual-client repository provenance")
	}
	capture.Trace.ClientProvenance = lr.inventoryProof
	data, err := json.Marshal(capture)
	if err != nil {
		return "", err
	}
	out := filepath.Join(c.Dir(), base+"-uploads.sanitized.json")
	if err := os.WriteFile(out, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	if err := c.Artifact("uploads-"+base, out); err != nil {
		return "", err
	}
	return out, nil
}
