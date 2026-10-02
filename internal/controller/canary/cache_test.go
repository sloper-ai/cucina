// SPDX-License-Identifier: FSL-1.1-ALv2

package canary_test

import (
	"context"
	"net"
	"sync"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/sloper-ai/cucina/internal/controller/canary"
)

// fakeCache is a minimal in-memory REAPI cache (Capabilities, CAS, AC) that
// requires the bearer token and can be told to corrupt reads.
type fakeCache struct {
	repb.UnimplementedCapabilitiesServer
	repb.UnimplementedContentAddressableStorageServer
	repb.UnimplementedActionCacheServer

	mu      sync.Mutex
	blobs   map[string][]byte
	ac      map[string]*repb.ActionResult
	corrupt bool
}

func (f *fakeCache) auth(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("authorization"); len(v) != 1 || v[0] != "Bearer good-token" {
		return status.Error(codes.Unauthenticated, "no valid token")
	}
	return nil
}

func (f *fakeCache) GetCapabilities(ctx context.Context, _ *repb.GetCapabilitiesRequest) (*repb.ServerCapabilities, error) {
	if err := f.auth(ctx); err != nil {
		return nil, err
	}
	return &repb.ServerCapabilities{CacheCapabilities: &repb.CacheCapabilities{DigestFunctions: []repb.DigestFunction_Value{repb.DigestFunction_SHA256}}}, nil
}

func (f *fakeCache) BatchUpdateBlobs(ctx context.Context, r *repb.BatchUpdateBlobsRequest) (*repb.BatchUpdateBlobsResponse, error) {
	if err := f.auth(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &repb.BatchUpdateBlobsResponse{}
	for _, b := range r.GetRequests() {
		f.blobs[b.GetDigest().GetHash()] = b.GetData()
		out.Responses = append(out.Responses, &repb.BatchUpdateBlobsResponse_Response{Digest: b.GetDigest(), Status: status.New(codes.OK, "").Proto()})
	}
	return out, nil
}

func (f *fakeCache) BatchReadBlobs(ctx context.Context, r *repb.BatchReadBlobsRequest) (*repb.BatchReadBlobsResponse, error) {
	if err := f.auth(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &repb.BatchReadBlobsResponse{}
	for _, d := range r.GetDigests() {
		data, ok := f.blobs[d.GetHash()]
		st := status.New(codes.OK, "").Proto()
		if !ok {
			st = status.New(codes.NotFound, "blob not found").Proto()
		}
		if f.corrupt && len(data) > 0 {
			data = append([]byte{data[0] ^ 0xff}, data[1:]...)
		}
		out.Responses = append(out.Responses, &repb.BatchReadBlobsResponse_Response{Digest: d, Data: data, Status: st})
	}
	return out, nil
}

func (f *fakeCache) UpdateActionResult(ctx context.Context, r *repb.UpdateActionResultRequest) (*repb.ActionResult, error) {
	if err := f.auth(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ac[r.GetActionDigest().GetHash()] = proto.Clone(r.GetActionResult()).(*repb.ActionResult)
	return r.GetActionResult(), nil
}

func (f *fakeCache) GetActionResult(ctx context.Context, r *repb.GetActionResultRequest) (*repb.ActionResult, error) {
	if err := f.auth(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ar, ok := f.ac[r.GetActionDigest().GetHash()]; ok {
		return ar, nil
	}
	return nil, status.Error(codes.NotFound, "no such action")
}

// Guards R-TEST-7 (synthetic cache canary) and R-CP-7 (helm test): the canary
// passes against a working cache and fails — with the failing step named —
// when the cache returns wrong content or rejects the token.
func TestCacheCanary(t *testing.T) {
	cases := []struct {
		name     string
		token    string
		corrupt  bool
		failStep string
	}{
		{"healthy cache", "good-token", false, ""},
		{"corrupted CAS read", "good-token", true, "cas-read"},
		{"token rejected", "bad-token", false, "get-capabilities"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeCache{blobs: map[string][]byte{}, ac: map[string]*repb.ActionResult{}, corrupt: tc.corrupt}
			lis := bufconn.Listen(1 << 20)
			srv := grpc.NewServer()
			repb.RegisterCapabilitiesServer(srv, fc)
			repb.RegisterContentAddressableStorageServer(srv, fc)
			repb.RegisterActionCacheServer(srv, fc)
			go func() { _ = srv.Serve(lis) }()
			t.Cleanup(srv.Stop)

			rep, err := canary.RunCache(context.Background(), canary.Options{
				Endpoint: "passthrough:///bufnet",
				Token:    tc.token,
				DialOptions: []grpc.DialOption{
					grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				},
			})
			if tc.failStep == "" {
				require.NoError(t, err)
				assert.True(t, rep.OK)
				var names []string
				for _, s := range rep.Steps {
					names = append(names, s.Name)
				}
				assert.Equal(t, []string{"get-capabilities", "cas-write", "cas-read", "ac-write", "ac-read"}, names)
				return
			}
			require.Error(t, err)
			assert.False(t, rep.OK)
			last := rep.Steps[len(rep.Steps)-1]
			assert.Equal(t, tc.failStep, last.Name)
			assert.NotEmpty(t, last.Error)
		})
	}
}
