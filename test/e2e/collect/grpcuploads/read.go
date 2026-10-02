// SPDX-License-Identifier: FSL-1.1-ALv2

package grpcuploads

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxFrame = 32 << 20
const maxEntries = 2_000_000

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var uploadRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var idRE = regexp.MustCompile(`^[A-Za-z0-9_.+~:@-]{1,240}$`)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func ParseInventory(b []byte) (Inventory, error) {
	var i Inventory
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&i); err != nil {
		return i, fmt.Errorf("invalid repository inventory")
	}
	return i, i.Validate()
}
func (i Inventory) Validate() error {
	if i.SchemaVersion != 1 || len(i.Repositories) == 0 {
		return fmt.Errorf("repository inventory schema 1 and nonempty repositories required")
	}
	seen := map[string]bool{}
	for _, r := range i.Repositories {
		if !idRE.MatchString(r.Name) || r.Component == "" || r.Version == "" || r.Variant == "" || r.Pin == "" || seen[r.Name] {
			return fmt.Errorf("inventory needs unique canonical names and explicit component/version/variant/pin")
		}
		seen[r.Name] = true
	}
	return nil
}

// Fingerprint binds sanitized evidence to the explicit inventory, including
// version and immutable source identity; changing the labels cannot relabel
// historical payload as a different release.
func (i Inventory) Fingerprint() string {
	copy := i
	copy.Repositories = append([]Repository(nil), i.Repositories...)
	sort.Slice(copy.Repositories, func(a, b int) bool { return copy.Repositories[a].Name < copy.Repositories[b].Name })
	b, _ := json.Marshal(copy)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type field struct {
	n   protowire.Number
	typ protowire.Type
	b   []byte
	v   uint64
}

func fields(b []byte, fn func(field) error) error {
	for len(b) > 0 {
		n, t, k := protowire.ConsumeTag(b)
		if k < 0 {
			return fmt.Errorf("invalid protobuf tag")
		}
		b = b[k:]
		f := field{n: n, typ: t}
		switch t {
		case protowire.BytesType:
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				return fmt.Errorf("invalid protobuf bytes")
			}
			f.b = v
			b = b[k:]
		case protowire.VarintType:
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				return fmt.Errorf("invalid protobuf integer")
			}
			f.v = v
			b = b[k:]
		default:
			k := protowire.ConsumeFieldValue(n, t, b)
			if k < 0 {
				return fmt.Errorf("invalid protobuf field")
			}
			b = b[k:]
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}
func bytesField(b []byte, n protowire.Number) ([]byte, error) {
	var out []byte
	err := fields(b, func(f field) error {
		if f.n == n {
			if f.typ != protowire.BytesType {
				return fmt.Errorf("wrong protobuf field type")
			}
			out = f.b
		}
		return nil
	})
	return out, err
}
func number(b []byte, n protowire.Number) (int64, error) {
	var out int64
	err := fields(b, func(f field) error {
		if f.n == n {
			if f.typ != protowire.VarintType {
				return fmt.Errorf("wrong protobuf number type")
			}
			out = int64(f.v)
		}
		return nil
	})
	return out, err
}
func timestamp(b []byte) (time.Time, error) {
	var p timestamppb.Timestamp
	if err := proto.Unmarshal(b, &p); err != nil {
		return time.Time{}, fmt.Errorf("malformed log timestamp")
	}
	if err := p.CheckValid(); err != nil {
		return time.Time{}, fmt.Errorf("invalid log timestamp")
	}
	return p.AsTime(), nil
}
func digest(d *repb.Digest) (Digest, error) {
	if d == nil || !hashRE.MatchString(d.Hash) || d.SizeBytes < 0 {
		return Digest{}, fmt.Errorf("invalid digest")
	}
	return Digest{Hash: d.Hash, Size: d.SizeBytes}, nil
}

// repositoryCommandDigest mirrors the immutable synthetic Command in
// Bazel9.2 RemoteRepoContentsCacheImpl (including the explicit empty Platform).
// It is protocol housekeeping, not compiler/SDK file content.
func repositoryCommandDigest() Digest {
	// Exact pinned Command wire fields, including deprecated output lists:
	// 1 arguments, 3 output_files, 4 output_directories, 5 platform, 7 output_paths.
	var b []byte
	for _, f := range []struct {
		n protowire.Number
		s string
	}{{1, "0336b325-9db8-4592-a5eb-79b4970bc4ce"}, {3, ".recorded_inputs"}, {4, "repo_contents"}, {5, ""}, {7, ".recorded_inputs"}, {7, "repo_contents"}} {
		b = protowire.AppendBytes(protowire.AppendTag(b, f.n, protowire.BytesType), []byte(f.s))
	}
	h := sha256.Sum256(b)
	return Digest{Hash: hex.EncodeToString(h[:]), Size: int64(len(b))}
}

type manifestState struct {
	Manifest
	all      map[string]Digest
	metadata map[string]Digest
	fmb      bool
}

// Read sanitizes the pinned LogEntry wire contract. Only known, explicitly
// inventoried repository identifiers survive. Unknown fields and arbitrary
// request/response bodies are discarded, never copied into Trace.
func Read(r io.Reader, o Options) (Trace, error) {
	t := Trace{SchemaVersion: 1, InventoryDigest: o.Inventory.Fingerprint(), Config: o.Config, InvocationID: o.InvocationID, Scope: o.Scope, HashFunction: strings.ReplaceAll(strings.ToUpper(o.HashFunction), "-", "")}
	if err := o.Inventory.Validate(); err != nil {
		return t, err
	}
	if !idRE.MatchString(o.Config) || !idRE.MatchString(o.InvocationID) || !hashRE.MatchString(o.Scope) || t.HashFunction != "SHA256" {
		return t, fmt.Errorf("known config/invocation, opaque CAS scope and SHA256 are required")
	}
	known := map[string]bool{}
	for _, repo := range o.Inventory.Repositories {
		known[repo.Name] = true
	}
	manifests := map[string]*manifestState{}
	getManifest := func(name string) *manifestState {
		m := manifests[name]
		if m == nil {
			m = &manifestState{Manifest: Manifest{Repository: name}, all: map[string]Digest{}, metadata: map[string]Digest{}}
			command := repositoryCommandDigest()
			m.metadata[command.Key()] = command
			manifests[name] = m
		}
		return m
	}
	br := bufio.NewReader(r)
	for count := 0; ; count++ {
		n, err := binary.ReadUvarint(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return t, fmt.Errorf("truncated RPC log length")
		}
		if n == 0 || n > maxFrame || count >= maxEntries {
			return t, fmt.Errorf("RPC log size limit exceeded")
		}
		b := make([]byte, int(n))
		if _, err := io.ReadFull(br, b); err != nil {
			return t, fmt.Errorf("truncated RPC log frame")
		}
		var meta repb.RequestMetadata
		var method string
		var detail []byte
		var started, finished time.Time
		var status int32
		statusSeen := false
		err = fields(b, func(f field) error {
			if f.n >= 1 && f.n <= 6 && f.typ != protowire.BytesType {
				return fmt.Errorf("wrong LogEntry field type")
			}
			switch f.n {
			case 1:
				if err := proto.Unmarshal(f.b, &meta); err != nil {
					return fmt.Errorf("malformed request metadata")
				}
			case 2:
				statusSeen = true
				v, err := number(f.b, 1)
				if err != nil {
					return err
				}
				if v < 0 || v > 16 {
					return fmt.Errorf("invalid RPC status code")
				}
				status = int32(v) // discard status message/details
			case 3:
				method = string(f.b)
			case 4:
				detail = f.b
			case 5:
				v, err := timestamp(f.b)
				if err != nil {
					return err
				}
				started = v
			case 6:
				v, err := timestamp(f.b)
				if err != nil {
					return err
				}
				finished = v
			}
			return nil
		})
		if err != nil {
			return t, err
		}
		if meta.ToolInvocationId == o.InvocationID {
			t.Entries++
		}
		isWrite := method == "google.bytestream.ByteStream/Write"
		isFMB := method == "build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs"
		isAC := method == "build.bazel.remote.execution.v2.ActionCache/UpdateActionResult"
		isQuery := method == "google.bytestream.ByteStream/QueryWriteStatus"
		if strings.HasSuffix(method, "/SpliceBlob") || strings.HasSuffix(method, "/SplitBlob") || strings.HasSuffix(method, "/BatchUpdateBlobs") {
			t.Issues = append(t.Issues, "unsupported chunking/batch upload evidence")
			continue
		}
		if !isWrite && !isFMB && !isAC && !isQuery {
			continue
		}
		if !statusSeen {
			t.Issues = append(t.Issues, "RPC completion status missing")
			continue
		}
		if meta.ToolInvocationId != o.InvocationID {
			t.Issues = append(t.Issues, "RPC invocation identity missing or mismatched")
			continue
		}
		if started.IsZero() || finished.Before(started) {
			return t, fmt.Errorf("invalid RPC time interval")
		}
		repo := ""
		if known[meta.ActionId] {
			repo = meta.ActionId
		}
		if isFMB && repo != "" {
			d, err := bytesField(detail, 10)
			if err != nil {
				return t, err
			}
			request, err := bytesField(d, 1)
			if err != nil {
				return t, err
			}
			var req repb.FindMissingBlobsRequest
			if err := proto.Unmarshal(request, &req); err != nil {
				return t, fmt.Errorf("malformed FindMissingBlobs request")
			}
			if req.InstanceName != o.Instance || (req.DigestFunction != repb.DigestFunction_UNKNOWN && req.DigestFunction != repb.DigestFunction_SHA256) {
				t.Issues = append(t.Issues, "repository manifest CAS scope mismatch")
				continue
			}
			if status != 0 {
				t.Issues = append(t.Issues, "repository manifest RPC failed")
				continue
			}
			m := getManifest(repo)
			m.fmb = true
			for _, x := range req.BlobDigests {
				v, err := digest(x)
				if err != nil {
					return t, err
				}
				m.all[v.Key()] = v
			}
		}
		if isAC {
			d, err := bytesField(detail, 13)
			if err != nil {
				return t, err
			}
			request, err := bytesField(d, 1)
			if err != nil {
				return t, err
			}
			var req repb.UpdateActionResultRequest
			if err := proto.Unmarshal(request, &req); err != nil {
				return t, fmt.Errorf("malformed action-cache request")
			}
			isRepo := false
			for _, dir := range req.GetActionResult().GetOutputDirectories() {
				if dir.Path == "repo_contents" {
					isRepo = true
				}
			}
			if repo == "" {
				if isRepo && isToolchainRepo(meta.ActionId) {
					t.Issues = append(t.Issues, "toolchain repository cache record lacks explicit inventory mapping")
				}
				continue
			}
			if req.InstanceName != o.Instance || status != 0 || (req.DigestFunction != repb.DigestFunction_UNKNOWN && req.DigestFunction != repb.DigestFunction_SHA256) {
				t.Issues = append(t.Issues, "repository cache completion not verified")
				continue
			}
			m := getManifest(repo)
			action, err := digest(req.ActionDigest)
			if err != nil {
				return t, fmt.Errorf("invalid repository action digest")
			}
			m.metadata[action.Key()] = action
			ar := req.GetActionResult()
			if ar == nil || ar.ExitCode != 0 {
				t.Issues = append(t.Issues, "repository action result missing or unsuccessful")
				continue
			}
			if ar.StdoutDigest != nil {
				d, err := digest(ar.StdoutDigest)
				if err != nil {
					return t, err
				}
				m.metadata[d.Key()] = d
			}
			if !isRepo {
				continue
			} // intermediate repository action/stdout are still housekeeping
			if len(ar.OutputFiles) != 1 || len(ar.OutputDirectories) != 1 || ar.OutputFiles[0].Path != ".recorded_inputs" || ar.OutputDirectories[0].Path != "repo_contents" {
				t.Issues = append(t.Issues, "repository result lacks exact marker/tree shape")
				continue
			}
			marker, err := digest(ar.OutputFiles[0].Digest)
			if err != nil {
				return t, err
			}
			m.metadata[marker.Key()] = marker
			tree, err := digest(ar.OutputDirectories[0].TreeDigest)
			if err != nil {
				return t, err
			}
			m.metadata[tree.Key()] = tree
			m.Tree = tree
			_, hasTree := m.all[tree.Key()]
			_, hasMarker := m.all[marker.Key()]
			m.Complete = m.fmb && hasTree && hasMarker
		}
		if isQuery {
			d, err := bytesField(detail, 14)
			if err != nil {
				return t, err
			}
			req, err := bytesField(d, 1)
			if err != nil {
				return t, err
			}
			name, err := bytesField(req, 1)
			if err != nil {
				return t, err
			}
			resource, err := parseResource(string(name), o.Instance)
			if err != nil {
				return t, err
			}
			response, err := bytesField(d, 2)
			if err != nil {
				return t, err
			}
			committed, err := number(response, 1)
			if err != nil {
				return t, err
			}
			complete, err := number(response, 2)
			if err != nil || complete < 0 || complete > 1 {
				return t, fmt.Errorf("invalid QueryWriteStatus completion")
			}
			t.Queries = append(t.Queries, Query{Digest: resource.Digest, Upload: resource.Upload, Compressor: resource.Compressor, CommittedSize: committed, Complete: complete == 1, Status: status})
		}
		if isWrite {
			d, err := bytesField(detail, 6)
			if err != nil {
				return t, err
			}
			w, err := readWrite(d, o.Instance)
			if err != nil {
				return t, err
			}
			if w.Upload == "" {
				if w.OfferedBytes != 0 {
					t.Issues = append(t.Issues, "payload without a CAS upload identity")
				}
				continue
			}
			w.Repository, w.Status, w.Started, w.Finished = repo, status, started, finished
			t.Writes = append(t.Writes, w)
		}
	}
	keys := make([]string, 0, len(manifests))
	for k := range manifests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := manifests[k]
		for _, d := range m.all {
			m.Digests = append(m.Digests, d)
		}
		for _, d := range m.metadata {
			m.Housekeeping = append(m.Housekeeping, d)
		}
		sortDigests(m.Digests)
		sortDigests(m.Housekeeping)
		t.Manifests = append(t.Manifests, m.Manifest)
	}
	return t, nil
}
func sortDigests(ds []Digest) {
	sort.Slice(ds, func(i, j int) bool { return ds[i].Key() < ds[j].Key() })
}

func readWrite(b []byte, instance string) (Write, error) {
	var w Write
	var names []string
	err := fields(b, func(f field) error {
		switch f.n {
		case 1:
			if len(f.b) > 0 {
				names = append(names, string(f.b))
			}
		case 3:
			if f.typ != protowire.VarintType {
				return fmt.Errorf("invalid payload count field")
			}
			w.OfferedBytes = int64(f.v)
		case 4:
			n, err := number(f.b, 1)
			if err != nil {
				return err
			}
			w.CommittedSize = &n
		case 5, 6:
			var ns []int64
			switch f.typ {
			case protowire.VarintType:
				ns = []int64{int64(f.v)}
			case protowire.BytesType:
				v := f.b
				for len(v) > 0 {
					n, k := protowire.ConsumeVarint(v)
					if k < 0 {
						return fmt.Errorf("bad packed offsets")
					}
					ns = append(ns, int64(n))
					v = v[k:]
				}
			default:
				return fmt.Errorf("invalid write offset encoding")
			}
			if f.n == 5 {
				w.Offsets = append(w.Offsets, ns...)
			} else {
				w.FinishWrites = append(w.FinishWrites, ns...)
			}
		}
		return nil
	})
	if err != nil {
		return w, err
	}
	if w.OfferedBytes < 0 {
		return w, fmt.Errorf("negative offered payload")
	}
	for _, n := range append(append([]int64{}, w.Offsets...), w.FinishWrites...) {
		if n < 0 {
			return w, fmt.Errorf("negative upload offset")
		}
	}
	if len(names) == 0 {
		return w, nil
	}
	for _, n := range names {
		if n != names[0] {
			return w, fmt.Errorf("upload changed resource identity")
		}
	}
	resource, err := parseResource(names[0], instance)
	if err != nil {
		return w, err
	}
	w.Upload, w.Digest, w.Compressor = resource.Upload, resource.Digest, resource.Compressor
	return w, nil
}

func parseResource(name, instance string) (Write, error) {
	var w Write
	prefix := strings.Trim(instance, "/")
	if prefix != "" {
		prefix += "/"
	}
	prefix += "uploads/"
	tail, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return w, fmt.Errorf("upload outside configured instance")
	}
	parts := strings.Split(tail, "/")
	if len(parts) < 4 || !uploadRE.MatchString(parts[0]) {
		return w, fmt.Errorf("invalid upload resource")
	}
	w.Upload = parts[0]
	parts = parts[1:]
	switch parts[0] {
	case "blobs":
		w.Compressor = "identity"
		parts = parts[1:]
	case "compressed-blobs":
		if len(parts) < 4 || parts[1] != "zstd" {
			return w, fmt.Errorf("unsupported blob compression")
		}
		w.Compressor = "zstd"
		parts = parts[2:]
	default:
		return w, fmt.Errorf("unsupported upload resource")
	}
	if len(parts) == 3 && parts[0] == "sha256" {
		parts = parts[1:]
	}
	if len(parts) != 2 || !hashRE.MatchString(parts[0]) {
		return w, fmt.Errorf("invalid upload digest")
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || n < 0 {
		return w, fmt.Errorf("invalid digest size")
	}
	w.Digest = Digest{Hash: parts[0], Size: n}
	return w, nil
}
