// SPDX-License-Identifier: FSL-1.1-ALv2

package bbtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// RequestMetadataKey is the gRPC metadata key carrying REAPI RequestMetadata.
const RequestMetadataKey = "build.bazel.remote.execution.v2.requestmetadata-bin"

// REClient is a minimal REAPI v2 client (SHA-256): enough to upload an action,
// execute it and read its outputs, the way Bazel would.
type REClient struct {
	Instance string
	cas      remoteexecution.ContentAddressableStorageClient
	exec     remoteexecution.ExecutionClient
}

// NewREClient talks to conn (a frontend or a scheduler client listener).
func NewREClient(conn grpc.ClientConnInterface, instance string) *REClient {
	return &REClient{
		Instance: instance,
		cas:      remoteexecution.NewContentAddressableStorageClient(conn),
		exec:     remoteexecution.NewExecutionClient(conn),
	}
}

// Action describes a command to run remotely.
type Action struct {
	Args        []string
	Env         map[string]string
	Inputs      map[string][]byte // file name → content, in the input root
	Executables []string          // names in Inputs that are executable
	OutputPaths []string
	Platform    map[string]string
	Timeout     time.Duration
	DoNotCache  bool
}

// DigestOf returns the SHA-256 digest of b.
func DigestOf(b []byte) *remoteexecution.Digest {
	h := sha256.Sum256(b)
	return &remoteexecution.Digest{Hash: hex.EncodeToString(h[:]), SizeBytes: int64(len(b))}
}

// Upload stores the action, its command and its (flat) input root in the CAS
// and returns the action digest.
func (c *REClient) Upload(ctx context.Context, a Action) (*remoteexecution.Digest, error) {
	blobs := map[string][]byte{}
	put := func(b []byte) *remoteexecution.Digest {
		d := DigestOf(b)
		blobs[d.Hash] = b
		return d
	}
	dir := &remoteexecution.Directory{}
	for _, name := range sortedKeys(a.Inputs) {
		dir.Files = append(dir.Files, &remoteexecution.FileNode{
			Name:         name,
			Digest:       put(a.Inputs[name]),
			IsExecutable: slices.Contains(a.Executables, name),
		})
	}
	cmd := &remoteexecution.Command{Arguments: a.Args, OutputPaths: a.OutputPaths}
	for _, k := range sortedKeys(a.Env) {
		cmd.EnvironmentVariables = append(cmd.EnvironmentVariables, &remoteexecution.Command_EnvironmentVariable{Name: k, Value: a.Env[k]})
	}
	action := &remoteexecution.Action{
		CommandDigest:   put(mustMarshal(cmd)),
		InputRootDigest: put(mustMarshal(dir)),
		DoNotCache:      a.DoNotCache,
		Platform:        Platform(a.Platform),
	}
	if a.Timeout > 0 {
		action.Timeout = durationpb.New(a.Timeout)
	}
	actionDigest := put(mustMarshal(action))
	req := &remoteexecution.BatchUpdateBlobsRequest{InstanceName: c.Instance, DigestFunction: remoteexecution.DigestFunction_SHA256}
	for _, h := range sortedKeys(blobs) {
		req.Requests = append(req.Requests, &remoteexecution.BatchUpdateBlobsRequest_Request{Digest: DigestOf(blobs[h]), Data: blobs[h]})
	}
	resp, err := c.cas.BatchUpdateBlobs(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("BatchUpdateBlobs: %w", err)
	}
	for _, r := range resp.Responses {
		if err := status.ErrorProto(r.Status); err != nil {
			return nil, fmt.Errorf("upload %s: %w", r.Digest.GetHash(), err)
		}
	}
	return actionDigest, nil
}

// Execution is a running Execute call.
type Execution struct {
	stream remoteexecution.Execution_ExecuteClient
	// Name is the operation name the scheduler assigned.
	Name string
	// done/resp/err hold the outcome when the first message already completed it.
	done bool
	resp *remoteexecution.ExecuteResponse
	err  error
}

// Start calls Execute and returns once the scheduler has accepted the action
// (first Operation message). md, when set, is sent as REAPI RequestMetadata.
func (c *REClient) Start(ctx context.Context, actionDigest *remoteexecution.Digest, md *remoteexecution.RequestMetadata) (*Execution, error) {
	if md != nil {
		ctx = metadata.AppendToOutgoingContext(ctx, RequestMetadataKey, string(mustMarshal(md)))
	}
	stream, err := c.exec.Execute(ctx, &remoteexecution.ExecuteRequest{
		InstanceName:    c.Instance,
		ActionDigest:    actionDigest,
		SkipCacheLookup: true,
		DigestFunction:  remoteexecution.DigestFunction_SHA256,
	})
	if err != nil {
		return nil, err
	}
	op, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("Execute: %w", err)
	}
	e := &Execution{stream: stream, Name: op.GetName()}
	if op.GetDone() {
		e.done = true
		e.resp, e.err = finish(status.ErrorProto(op.GetError()), op.GetResponse())
	}
	return e, nil
}

// Wait blocks until the operation completes and returns its ExecuteResponse.
// A non-OK ExecuteResponse.status is returned as an error.
func (e *Execution) Wait() (*remoteexecution.ExecuteResponse, error) {
	if e.done {
		return e.resp, e.err
	}
	for {
		op, err := e.stream.Recv()
		if err != nil {
			return nil, fmt.Errorf("Execute stream: %w", err)
		}
		if op.GetDone() {
			return finish(status.ErrorProto(op.GetError()), op.GetResponse())
		}
	}
}

func finish(opErr error, response *anypb.Any) (*remoteexecution.ExecuteResponse, error) {
	if opErr != nil {
		return nil, opErr
	}
	resp := &remoteexecution.ExecuteResponse{}
	if err := response.UnmarshalTo(resp); err != nil {
		return nil, err
	}
	if err := status.ErrorProto(resp.GetStatus()); err != nil {
		return resp, err
	}
	return resp, nil
}

// Execute uploads nothing: it runs a previously uploaded action to completion.
func (c *REClient) Execute(ctx context.Context, actionDigest *remoteexecution.Digest, md *remoteexecution.RequestMetadata) (*remoteexecution.ExecuteResponse, error) {
	e, err := c.Start(ctx, actionDigest, md)
	if err != nil {
		return nil, err
	}
	return e.Wait()
}

// Read fetches a blob from the CAS.
func (c *REClient) Read(ctx context.Context, d *remoteexecution.Digest) ([]byte, error) {
	resp, err := c.cas.BatchReadBlobs(ctx, &remoteexecution.BatchReadBlobsRequest{
		InstanceName:   c.Instance,
		Digests:        []*remoteexecution.Digest{d},
		DigestFunction: remoteexecution.DigestFunction_SHA256,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Responses) != 1 {
		return nil, errors.New("BatchReadBlobs returned no response")
	}
	if err := status.ErrorProto(resp.Responses[0].Status); err != nil {
		return nil, err
	}
	return resp.Responses[0].Data, nil
}

// OutputFile returns the content of an output file of resp.
func (c *REClient) OutputFile(ctx context.Context, resp *remoteexecution.ExecuteResponse, path string) ([]byte, error) {
	var names []string
	for _, f := range resp.GetResult().GetOutputFiles() {
		if f.Path == path {
			return c.Read(ctx, f.Digest)
		}
		names = append(names, f.Path)
	}
	return nil, fmt.Errorf("no output file %q (have %s)", path, strings.Join(names, ", "))
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }
