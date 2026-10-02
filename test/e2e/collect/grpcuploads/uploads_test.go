// SPDX-License-Identifier: FSL-1.1-ALv2

package grpcuploads_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sloper-ai/cucina/test/e2e/collect/grpcuploads"
)

func wireBytes(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func wireInt(n protowire.Number, v int64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), uint64(v))
}
func message(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}
func join(bs ...[]byte) []byte { return bytes.Join(bs, nil) }
func dig(n int, size int64) *repb.Digest {
	return &repb.Digest{Hash: fmt.Sprintf("%064x", n), SizeBytes: size}
}
func plain(d *repb.Digest) grpcuploads.Digest {
	return grpcuploads.Digest{Hash: d.Hash, Size: d.SizeBytes}
}
func inventory() grpcuploads.Inventory {
	return grpcuploads.Inventory{SchemaVersion: 1, Repositories: []grpcuploads.Repository{{Name: "llvm++repo+linux", Component: "llvm", Version: "23.1.2", Variant: "linux-x86_64", Pin: "sha256:immutable-source"}}}
}
func options(config string) grpcuploads.Options {
	return grpcuploads.Options{Config: config, InvocationID: "inv-" + config, Scope: strings.Repeat("a", 64), Instance: "main", HashFunction: "SHA256", Inventory: inventory()}
}
func entry(config, repo, method string, which protowire.Number, details []byte, status int32) []byte {
	meta := message(&repb.RequestMetadata{ToolInvocationId: "inv-" + config, ActionId: repo, TargetId: "private-secret-sentinel"})
	ts := message(timestamppb.New(time.Unix(1_800_000_000, 0)))
	b := join(wireBytes(1, meta), wireBytes(2, join(wireInt(1, int64(status)), wireBytes(2, []byte("private-secret-sentinel")))), wireBytes(3, []byte(method)), wireBytes(4, wireBytes(which, details)), wireBytes(5, ts), wireBytes(6, ts), wireBytes(99, []byte("private-secret-sentinel")))
	var buf bytes.Buffer
	var length [10]byte
	n := binary.PutUvarint(length[:], uint64(len(b)))
	buf.Write(length[:n])
	buf.Write(b)
	return buf.Bytes()
}
func write(config, repo, upload string, d *repb.Digest, offered int64, status int32) []byte {
	h := sha256.Sum256([]byte(upload))
	u := hex.EncodeToString(h[:16])
	upload = u[:8] + "-" + u[8:12] + "-" + u[12:16] + "-" + u[16:20] + "-" + u[20:]
	name := fmt.Sprintf("main/uploads/%s/compressed-blobs/zstd/%s/%d", upload, d.Hash, d.SizeBytes)
	body := join(wireBytes(1, []byte(name)), wireBytes(1, nil), wireInt(2, 1), wireInt(3, offered), wireInt(5, 0), wireInt(6, offered), wireBytes(4, wireInt(1, offered)), wireBytes(99, []byte("private-secret-sentinel")))
	return entry(config, repo, "google.bytestream.ByteStream/Write", 6, body, status)
}
func seed(config, repo string) []byte {
	file, tree, marker, action := dig(1, 100), dig(2, 20), dig(3, 4), dig(4, 8)
	fmb := entry(config, repo, "build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs", 10, wireBytes(1, message(&repb.FindMissingBlobsRequest{InstanceName: "main", BlobDigests: []*repb.Digest{file, tree, marker, action}})), 0)
	ac := entry(config, repo, "build.bazel.remote.execution.v2.ActionCache/UpdateActionResult", 13, wireBytes(1, message(&repb.UpdateActionResultRequest{InstanceName: "main", ActionDigest: action, ActionResult: &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: ".recorded_inputs", Digest: marker}}, OutputDirectories: []*repb.OutputDirectory{{Path: "repo_contents", TreeDigest: tree}}, StdoutRaw: []byte("private-secret-sentinel")}})), 0)
	return join(fmb, write(config, repo, "first", file, 40, 0), ac)
}

// Guards §12/X5's sanitizer boundary: the public summary retains actual offered
// payload/digest/manifest evidence, never arbitrary metadata, status text or
// action-result contents. Truncation and CAS-scope mismatches cannot pass.
func TestRPCSanitization(t *testing.T) {
	opt := options("seed")
	raw := seed("seed", opt.Inventory.Repositories[0].Name)
	tr, err := grpcuploads.Read(bytes.NewReader(raw), opt)
	require.NoError(t, err)
	require.Len(t, tr.Manifests, 1)
	require.True(t, tr.Manifests[0].Complete)
	require.Len(t, tr.Manifests[0].Digests, 4)
	require.Len(t, tr.Writes, 1)
	require.Equal(t, int64(40), tr.Writes[0].OfferedBytes)
	require.Equal(t, int64(100), tr.Writes[0].Digest.Size)
	b, err := json.Marshal(tr)
	require.NoError(t, err)
	require.NotContains(t, string(b), "private-secret-sentinel")
	_, err = grpcuploads.Read(bytes.NewReader(raw[:len(raw)-1]), opt)
	require.Error(t, err)
	wrong := opt
	wrong.Instance = "other"
	_, err = grpcuploads.Read(bytes.NewReader(raw), wrong)
	require.Error(t, err)
	tr, err = grpcuploads.Read(bytes.NewReader(write("seed", opt.Inventory.Repositories[0].Name, "already", dig(1, 100), 17, 6)), opt)
	require.NoError(t, err)
	require.Equal(t, int64(17), tr.Writes[0].OfferedBytes, "ALREADY_EXISTS does not erase bytes already offered")
}

// Guards explicit inventory provenance: versions come from resolved source
// attributes, not apparent/canonical names, and owner-rule changes alter pins.
func TestResolvedInventory(t *testing.T) {
	rules := `## @@llvm+:
http_archive(
  name = "llvm+",
  integrity = "sha256-source",
  remote_module_file_urls = ["https://bcr.bazel.build/modules/llvm/0.8.24/MODULE.bazel"],
)
## @@llvm++tools+compiler:
http_archive(
  name = "llvm++tools+compiler",
  sha256 = "immutable-source",
  urls = ["https://github.com/hermeticbuild/hermetic-llvm/releases/download/llvm-23.1.2-1/compiler.tar.zst"],
)
## @@unrelated+:
http_archive(name = "unrelated+")
`
	i, err := grpcuploads.InventoryFromRules(rules)
	require.NoError(t, err)
	require.Len(t, i.Repositories, 2)
	crlf, err := grpcuploads.InventoryFromRules(strings.ReplaceAll(rules, "\n", "\r\n"))
	require.NoError(t, err)
	require.Equal(t, i.Fingerprint(), crlf.Fingerprint(), "PowerShell line endings do not change resolved source identity")
	require.Equal(t, "0.8.24", i.Repositories[0].Version)
	require.Equal(t, "llvm-23.1.2-1", i.Repositories[1].Version)
	changed, err := grpcuploads.InventoryFromRules(strings.Replace(rules, "sha256-source", "sha256-different", 1))
	require.NoError(t, err)
	require.NotEqual(t, i.Repositories[1].Pin, changed.Repositories[1].Pin, "generated repo identity includes its owning module pin")
	require.NoError(t, grpcuploads.VerifyRepositoryPins(i, i, []string{i.Repositories[1].Name}))
	require.Error(t, grpcuploads.VerifyRepositoryPins(i, changed, []string{i.Repositories[1].Name}), "same canonical name cannot relabel newer client source as the supplied older pin")
	_, err = grpcuploads.InventoryFromRules("not a repository export")
	require.Error(t, err)
}

// Guards the pinned compressed-write and repository-result completion contract:
// housekeeping is not initial toolchain content and malformed success cannot pass.
func TestCompletionEvidence(t *testing.T) {
	inv := inventory()
	repo := inv.Repositories[0].Name
	file, tree, marker, action := dig(1, 100), dig(2, 20), dig(3, 4), dig(4, 8)
	cmdBytes := join(wireBytes(1, []byte("0336b325-9db8-4592-a5eb-79b4970bc4ce")), wireBytes(3, []byte(".recorded_inputs")), wireBytes(4, []byte("repo_contents")), wireBytes(5, nil), wireBytes(7, []byte(".recorded_inputs")), wireBytes(7, []byte("repo_contents")))
	h := sha256.Sum256(cmdBytes)
	cmd := &repb.Digest{Hash: hex.EncodeToString(h[:]), SizeBytes: int64(len(cmdBytes))}
	for _, tc := range []struct {
		name                                                                           string
		noMarker, badAction, badExit, badFunction, badWire, metadataOnly, badCommitted bool
		want                                                                           bool
	}{
		{name: "valid compressed finish", want: true},
		{name: "missing marker", noMarker: true},
		{name: "invalid action", badAction: true},
		{name: "failed action result", badExit: true},
		{name: "different digest function", badFunction: true},
		{name: "wrong wire status", badWire: true},
		{name: "only synthetic command uploaded", metadataOnly: true},
		{name: "logical size is not compressed committed size", badCommitted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fmb := entry("seed", repo, "build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs", 10, wireBytes(1, message(&repb.FindMissingBlobsRequest{InstanceName: "main", BlobDigests: []*repb.Digest{file, tree, marker, action, cmd}})), 0)
			result := &repb.ActionResult{OutputFiles: []*repb.OutputFile{{Path: ".recorded_inputs", Digest: marker}}, OutputDirectories: []*repb.OutputDirectory{{Path: "repo_contents", TreeDigest: tree}}}
			request := &repb.UpdateActionResultRequest{InstanceName: "main", ActionDigest: action, ActionResult: result}
			if tc.noMarker {
				result.OutputFiles = nil
			}
			if tc.badAction {
				request.ActionDigest = nil
			}
			if tc.badExit {
				result.ExitCode = 7
			}
			if tc.badFunction {
				request.DigestFunction = repb.DigestFunction_SHA1
			}
			ac := entry("seed", repo, "build.bazel.remote.execution.v2.ActionCache/UpdateActionResult", 13, wireBytes(1, message(request)), 0)
			if tc.badWire {
				_, n := binary.Uvarint(ac)
				body := append(append([]byte{}, ac[n:]...), wireInt(2, 0)...)
				var length [10]byte
				n = binary.PutUvarint(length[:], uint64(len(body)))
				ac = append(append([]byte{}, length[:n]...), body...)
			}
			d := file
			if tc.metadataOnly {
				d = cmd
			}
			committed := int64(40)
			if tc.badCommitted {
				committed = d.SizeBytes
			}
			resource := fmt.Sprintf("main/uploads/00000000-0000-4000-8000-000000000001/compressed-blobs/zstd/%s/%d", d.Hash, d.SizeBytes)
			w := entry("seed", repo, "google.bytestream.ByteStream/Write", 6, join(wireBytes(1, []byte(resource)), wireInt(3, 40), wireInt(5, 0), wireInt(6, 40), wireBytes(4, wireInt(1, committed))), 0)
			trace, err := grpcuploads.Read(bytes.NewReader(join(fmb, w, ac)), options("seed"))
			if err != nil {
				require.False(t, tc.want)
				return
			}
			trace.ClientProvenance = strings.Repeat("c", 64)
			warm := grpcuploads.Trace{SchemaVersion: 1, Config: "warm", InvocationID: "inv-warm", Scope: trace.Scope, HashFunction: "SHA256", Entries: 1, InventoryDigest: inv.Fingerprint(), ClientProvenance: strings.Repeat("c", 64)}
			r := grpcuploads.Evaluate(inv, []grpcuploads.Capture{{Trace: trace, Files: []grpcuploads.File{{Repository: repo, Digest: plain(file)}}}, {Trace: warm, Files: []grpcuploads.File{{Repository: repo, Digest: plain(file)}}}})
			require.Equal(t, tc.want, r.Pass)
		})
	}
}

// Guards X5: a full baseline plus observed reuse can pass; repeat-config
// uploads, incomplete baselines, unknown versions and empty logs cannot.
func TestUploadReuseEvidence(t *testing.T) {
	inv := inventory()
	repo := inv.Repositories[0].Name
	base, err := grpcuploads.Read(bytes.NewReader(seed("seed", repo)), options("seed"))
	require.NoError(t, err)
	base.ClientProvenance = strings.Repeat("c", 64)
	warm := grpcuploads.Trace{SchemaVersion: 1, Config: "warm", InvocationID: "inv-warm", Scope: base.Scope, HashFunction: "SHA256", Entries: 1, InventoryDigest: inv.Fingerprint(), ClientProvenance: strings.Repeat("c", 64)}
	used := []grpcuploads.File{{Repository: repo, Digest: plain(dig(1, 100))}}
	for _, tc := range []struct {
		name            string
		edit            func([]grpcuploads.Capture) []grpcuploads.Capture
		pass, violation bool
	}{
		{name: "complete observed reuse", pass: true},
		{name: "no actual client provenance", edit: func(c []grpcuploads.Capture) []grpcuploads.Capture { c[1].Trace.ClientProvenance = ""; return c }},
		{name: "no logs", edit: func([]grpcuploads.Capture) []grpcuploads.Capture { return nil }},
		{name: "no cold manifest", edit: func(c []grpcuploads.Capture) []grpcuploads.Capture { c[0].Trace.Manifests = nil; return c }},
		{name: "no exercised second config", edit: func(c []grpcuploads.Capture) []grpcuploads.Capture { c[1].Files = nil; return c }},
		{name: "different CAS", edit: func(c []grpcuploads.Capture) []grpcuploads.Capture {
			c[1].Trace.Scope = strings.Repeat("b", 64)
			return c
		}},
		{name: "repeat config payload", violation: true, edit: func(c []grpcuploads.Capture) []grpcuploads.Capture {
			tr, err := grpcuploads.Read(bytes.NewReader(write("warm", "ordinary-action", "repeat", dig(1, 100), 9, 6)), options("warm"))
			require.NoError(t, err)
			c[1].Trace = tr
			return c
		}},
		{name: "same configuration retry counted honestly", pass: true, edit: func(c []grpcuploads.Capture) []grpcuploads.Capture {
			w := c[0].Trace.Writes[0]
			w.Status = 14
			w.OfferedBytes = 7
			c[0].Trace.Writes = append([]grpcuploads.Write{w}, c[0].Trace.Writes...)
			return c
		}},
		{name: "resume confirmed by QueryWriteStatus", pass: true, edit: func(c []grpcuploads.Capture) []grpcuploads.Capture {
			c[0].Trace.Writes[0].Status = 14
			c[0].Trace.Queries = []grpcuploads.Query{{Digest: c[0].Trace.Writes[0].Digest, Upload: c[0].Trace.Writes[0].Upload, Compressor: "zstd", Status: 0, Complete: true, CommittedSize: 40}}
			return c
		}},
		{name: "invalid resume completion", edit: func(c []grpcuploads.Capture) []grpcuploads.Capture {
			c[0].Trace.Writes[0].Status = 14
			c[0].Trace.Queries = []grpcuploads.Query{{Digest: c[0].Trace.Writes[0].Digest, Upload: c[0].Trace.Writes[0].Upload, Compressor: "zstd", Status: 0, Complete: true, CommittedSize: 0}}
			return c
		}},
		{name: "resume compressor mismatch", edit: func(c []grpcuploads.Capture) []grpcuploads.Capture {
			c[0].Trace.Writes[0].Status = 14
			c[0].Trace.Queries = []grpcuploads.Query{{Digest: c[0].Trace.Writes[0].Digest, Upload: c[0].Trace.Writes[0].Upload, Compressor: "identity", Status: 0, Complete: true, CommittedSize: 40}}
			return c
		}},
		{name: "uncompleted upload", edit: func(c []grpcuploads.Capture) []grpcuploads.Capture { c[0].Trace.Writes[0].Status = 14; return c }},
		{name: "parser issue", edit: func(c []grpcuploads.Capture) []grpcuploads.Capture {
			c[1].Trace.Issues = []string{"partial log"}
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Mutations belong to this row, not the shared baseline slices.
			var c []grpcuploads.Capture
			data, err := json.Marshal([]grpcuploads.Capture{{Trace: base, Files: used}, {Trace: warm, Files: used}})
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &c))
			if tc.edit != nil {
				c = tc.edit(c)
			}
			r := grpcuploads.Evaluate(inv, c)
			require.Equal(t, tc.pass, r.Pass)
			if tc.pass {
				require.GreaterOrEqual(t, r.OfferedBytes, int64(40))
				require.Equal(t, int64(100), r.UniqueContentBytes)
			} else if tc.violation {
				require.NotEmpty(t, r.Violations)
			} else {
				require.NotEmpty(t, r.Unavailable)
			}
		})
	}
	// One physical blob shared by two explicitly pinned repositories is
	// credited once, but BOTH versions need observed reuse coverage.
	shared := inventory()
	second := shared.Repositories[0]
	second.Name = "sdk++repo+linux"
	second.Component = "sdk"
	shared.Repositories = append(shared.Repositories, second)
	manifest := base.Manifests[0]
	manifest.Repository = second.Name
	sharedBase := base
	sharedBase.InventoryDigest = shared.Fingerprint()
	sharedWarm := warm
	sharedWarm.InventoryDigest = shared.Fingerprint()
	sharedBase.Manifests = append(append([]grpcuploads.Manifest(nil), base.Manifests...), manifest)
	files := append(append([]grpcuploads.File(nil), used...), grpcuploads.File{Repository: second.Name, Digest: used[0].Digest})
	sharedReport := grpcuploads.Evaluate(shared, []grpcuploads.Capture{{Trace: sharedBase, Files: files}, {Trace: sharedWarm, Files: files}})
	require.True(t, sharedReport.Pass)
	require.Equal(t, int64(100), sharedReport.UniqueContentBytes)
	require.Equal(t, int64(40), sharedReport.OfferedBytes)
	partial := grpcuploads.Evaluate(shared, []grpcuploads.Capture{{Trace: sharedBase, Files: files}, {Trace: sharedWarm, Files: used}})
	require.False(t, partial.Pass)
	require.NotEmpty(t, partial.Unavailable)
	// A different warm-only variant cannot borrow LLVM's positive initial
	// upload. Shared ownership above may qualify both; disjoint content may not.
	disjoint := sharedBase
	disjoint.Manifests = append([]grpcuploads.Manifest(nil), sharedBase.Manifests...)
	disjoint.Manifests[1].Digests = []grpcuploads.Digest{plain(dig(50, 200)), plain(dig(2, 20)), plain(dig(3, 4))}
	disjointFiles := []grpcuploads.File{used[0], {Repository: second.Name, Digest: plain(dig(50, 200))}}
	r := grpcuploads.Evaluate(shared, []grpcuploads.Capture{{Trace: disjoint, Files: disjointFiles}, {Trace: sharedWarm, Files: disjointFiles}})
	require.False(t, r.Pass, "each assessed variant needs its own initial-content evidence")
	require.NotEmpty(t, r.Unavailable)

	bad := inv
	bad.Repositories = append([]grpcuploads.Repository(nil), inv.Repositories...)
	bad.Repositories[0].Version = ""
	require.NotEmpty(t, grpcuploads.Evaluate(bad, []grpcuploads.Capture{{Trace: base, Files: used}, {Trace: warm, Files: used}}).Unavailable)
}
