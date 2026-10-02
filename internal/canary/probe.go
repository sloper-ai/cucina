// SPDX-License-Identifier: FSL-1.1-ALv2

package canary

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Probe runs one canary.
type Probe struct {
	Kind     string
	Endpoint Endpoint
	Tokens   TokenSource // nil: unauthenticated (local tests only)
	// Pool and Platform select the exec canary's target: the exact runner
	// property set of the pool (Buildbarn matches the whole set).
	Pool     string
	Platform map[string]string
	// Timeout bounds the whole probe (default 2 min cache, 15 min exec: an
	// execution canary may scale a pool from zero, slow-path Windows ≈ 4 min).
	Timeout time.Duration
	Now     func() time.Time
}

func (p *Probe) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func nonce() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func digestOf(b []byte) *repb.Digest {
	return &repb.Digest{Hash: sha256Hex(b), SizeBytes: int64(len(b))}
}

// Run executes the probe and returns its result (never an error: failures
// are part of the result).
func (p *Probe) Run(ctx context.Context) Result {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
		if p.Kind == KindExec {
			timeout = 15 * time.Minute
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := p.now()
	rec := &recorder{r: &Result{Kind: p.Kind, Pool: p.Pool, Instance: p.Endpoint.InstanceName, Started: start}, now: p.now}
	err := p.run(ctx, rec)
	return rec.finish(start, err)
}

func (p *Probe) run(ctx context.Context, rec *recorder) error {
	var token string
	if p.Tokens != nil {
		if err := rec.step("token", func() error {
			t, err := p.Tokens.Token(ctx)
			token, rec.r.Subject = t.Raw, t.Subject
			return err
		}); err != nil {
			return err
		}
	}
	conn, err := p.Endpoint.Dial(token)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	inst := p.Endpoint.InstanceName
	caps := repb.NewCapabilitiesClient(conn)
	cas := repb.NewContentAddressableStorageClient(conn)

	if err := rec.step("capabilities", func() error {
		c, err := caps.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{InstanceName: inst})
		if err != nil {
			return err
		}
		cc := c.GetCacheCapabilities()
		if cc == nil {
			return errors.New("no cache capabilities")
		}
		sha := false
		for _, f := range cc.GetDigestFunctions() {
			sha = sha || f == repb.DigestFunction_SHA256
		}
		if !sha {
			return errors.New("SHA256 not advertised")
		}
		for _, c := range cc.GetSupportedCompressors() {
			rec.r.Compressors = append(rec.r.Compressors, c.String())
		}
		sort.Strings(rec.r.Compressors)
		return nil
	}); err != nil {
		return err
	}
	if p.Kind == KindExec {
		return p.exec(ctx, rec, conn, cas)
	}
	return p.cache(ctx, rec, repb.NewActionCacheClient(conn), cas)
}

// cache is the AC/CAS round trip.
func (p *Probe) cache(ctx context.Context, rec *recorder, ac repb.ActionCacheClient, cas repb.ContentAddressableStorageClient) error {
	inst := p.Endpoint.InstanceName
	n := nonce()
	blob := []byte("cucina cache canary " + n + " " + p.now().UTC().Format(time.RFC3339Nano))
	d := digestOf(blob)
	if err := rec.step("cas-write", func() error {
		r, err := cas.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: inst, Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: d, Data: blob}}})
		if err != nil {
			return err
		}
		return firstStatus(len(r.GetResponses()), func(i int) (int32, string) {
			return r.GetResponses()[i].GetStatus().GetCode(), r.GetResponses()[i].GetStatus().GetMessage()
		})
	}); err != nil {
		return err
	}
	if err := rec.step("cas-find-missing", func() error {
		r, err := cas.FindMissingBlobs(ctx, &repb.FindMissingBlobsRequest{InstanceName: inst, BlobDigests: []*repb.Digest{d}})
		if err != nil {
			return err
		}
		if len(r.GetMissingBlobDigests()) != 0 {
			return errors.New("blob just written is reported missing")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := rec.step("cas-read", func() error {
		r, err := cas.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: inst, Digests: []*repb.Digest{d}})
		if err != nil {
			return err
		}
		if len(r.GetResponses()) != 1 || r.GetResponses()[0].GetStatus().GetCode() != int32(codes.OK) {
			return fmt.Errorf("read failed: %v", r.GetResponses())
		}
		if !bytes.Equal(r.GetResponses()[0].GetData(), blob) {
			return errors.New("read back different content")
		}
		return nil
	}); err != nil {
		return err
	}
	// The AC entry is keyed by a synthetic action digest unique to this run
	// and references the CAS blob above as its only output, so frontend
	// completeness checking accepts it.
	actionKey := digestOf([]byte("cucina cache canary action " + n))
	want := &repb.ActionResult{
		ExitCode:    0,
		StdoutRaw:   []byte("canary " + n),
		OutputFiles: []*repb.OutputFile{{Path: "canary.txt", Digest: d}},
	}
	if err := rec.step("ac-write", func() error {
		_, err := ac.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: inst, ActionDigest: actionKey, ActionResult: want})
		return err
	}); err != nil {
		return err
	}
	return rec.step("ac-read", func() error {
		got, err := ac.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: inst, ActionDigest: actionKey, InlineStdout: true})
		if err != nil {
			return err
		}
		if len(got.GetOutputFiles()) != 1 || !proto.Equal(got.GetOutputFiles()[0].GetDigest(), d) {
			return fmt.Errorf("action result differs: %v", got.GetOutputFiles())
		}
		return nil
	})
}

func firstStatus(n int, get func(int) (int32, string)) error {
	if n == 0 {
		return errors.New("no responses")
	}
	for i := 0; i < n; i++ {
		if c, m := get(i); c != int32(codes.OK) {
			return fmt.Errorf("%s: %s", codes.Code(c), m)
		}
	}
	return nil
}

// exec runs a tiny action on the pool's runner without cache lookup.
func (p *Probe) exec(ctx context.Context, rec *recorder, conn *grpc.ClientConn, cas repb.ContentAddressableStorageClient) error {
	if len(p.Platform) == 0 {
		return errors.New("exec canary needs the pool's platform properties")
	}
	inst := p.Endpoint.InstanceName
	n := nonce()
	args := []string{"/bin/sh", "-c", "echo cucina-canary-" + n}
	if strings.EqualFold(p.Platform["OSFamily"], "windows") {
		args = []string{"cmd.exe", "/c", "echo cucina-canary-" + n}
	}
	var props []*repb.Platform_Property
	keys := make([]string, 0, len(p.Platform))
	for k := range p.Platform {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		props = append(props, &repb.Platform_Property{Name: k, Value: p.Platform[k]})
	}
	platform := &repb.Platform{Properties: props}
	// REAPI v2.2+: the platform lives on the Action (Buildbarn matches it there).
	cmdBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&repb.Command{Arguments: args})
	if err != nil {
		return err
	}
	dirBytes, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&repb.Directory{})
	action := &repb.Action{
		CommandDigest:   digestOf(cmdBytes),
		InputRootDigest: digestOf(dirBytes),
		Timeout:         durationpb.New(time.Minute),
		DoNotCache:      true,
		Platform:        platform,
	}
	actBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(action)
	if err != nil {
		return err
	}
	actionDigest := digestOf(actBytes)
	if err := rec.step("upload", func() error {
		r, err := cas.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: inst, Requests: []*repb.BatchUpdateBlobsRequest_Request{
			{Digest: action.CommandDigest, Data: cmdBytes}, {Digest: action.InputRootDigest, Data: dirBytes}, {Digest: actionDigest, Data: actBytes},
		}})
		if err != nil {
			return err
		}
		return firstStatus(len(r.GetResponses()), func(i int) (int32, string) {
			return r.GetResponses()[i].GetStatus().GetCode(), r.GetResponses()[i].GetStatus().GetMessage()
		})
	}); err != nil {
		return err
	}
	var resp repb.ExecuteResponse
	if err := rec.step("execute", func() error {
		stream, err := repb.NewExecutionClient(conn).Execute(ctx, &repb.ExecuteRequest{InstanceName: inst, ActionDigest: actionDigest, SkipCacheLookup: true})
		if err != nil {
			return err
		}
		for {
			op, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return errors.New("stream ended before the operation completed")
			}
			if err != nil {
				return err
			}
			if !op.GetDone() {
				continue
			}
			if e := op.GetError(); e != nil {
				return fmt.Errorf("operation failed: %s: %s", codes.Code(e.GetCode()), e.GetMessage())
			}
			return op.GetResponse().UnmarshalTo(&resp)
		}
	}); err != nil {
		return err
	}
	return rec.step("verify", func() error {
		if s := resp.GetStatus(); s.GetCode() != int32(codes.OK) {
			return fmt.Errorf("execute status %s: %s", codes.Code(s.GetCode()), s.GetMessage())
		}
		res := resp.GetResult()
		md := res.GetExecutionMetadata()
		rec.r.Worker = md.GetWorker()
		if q, w := md.GetQueuedTimestamp(), md.GetWorkerStartTimestamp(); q != nil && w != nil {
			rec.r.QueueTime = w.AsTime().Sub(q.AsTime())
		}
		if res.GetExitCode() != 0 {
			return fmt.Errorf("canary action exited %d", res.GetExitCode())
		}
		out := res.GetStdoutRaw()
		if len(out) == 0 && res.GetStdoutDigest() != nil {
			r, err := cas.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: inst, Digests: []*repb.Digest{res.GetStdoutDigest()}})
			if err != nil {
				return err
			}
			if len(r.GetResponses()) == 1 {
				out = r.GetResponses()[0].GetData()
			}
		}
		if !bytes.Contains(out, []byte("cucina-canary-"+n)) {
			return fmt.Errorf("stdout lacks the canary nonce: %q", truncate(string(out), 200))
		}
		return nil
	})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
