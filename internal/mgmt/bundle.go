// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/api/v1alpha1"
	cproto "github.com/sloper-ai/cucina/internal/proto"
)

const (
	bundleTimeout        = 60 * time.Second
	bundleLogLines       = 5000
	bundleMaxOperations  = 1000
	lastAppliedConfigKey = "kubectl.kubernetes.io/last-applied-configuration"
)

// bundle accumulates support-bundle files. It has no way to add unredacted content:
// every JSON document goes through RedactJSON and every text file through
// RedactText (secrets redacted by construction).
type bundle struct {
	files  []bundleFile
	errors map[string]string
}

type bundleFile struct {
	name string
	data []byte
}

func (b *bundle) fail(name string, err error) {
	if b.errors == nil {
		b.errors = map[string]string{}
	}
	b.errors[name] = RedactText(err.Error())
}

// addJSON adds v (marshalled with encoding/json, or protojson for messages), redacted.
func (b *bundle) addJSON(name string, v any) {
	var raw []byte
	var err error
	if m, ok := v.(proto.Message); ok {
		raw, err = protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	} else {
		raw, err = json.Marshal(v)
	}
	if err == nil {
		raw, err = RedactJSON(raw)
	}
	if err != nil {
		b.fail(name, err)
		return
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") == nil {
		raw = append(pretty.Bytes(), '\n')
	}
	b.files = append(b.files, bundleFile{name, raw})
}

// addText adds a text file, redacted.
func (b *bundle) addText(name string, data []byte) {
	b.files = append(b.files, bundleFile{name, RedactBytes(data)})
}

// write renders the bundle as tar.gz.
func (b *bundle) write(w io.Writer, now time.Time) error {
	if len(b.errors) > 0 {
		b.addJSON("errors.json", b.errors)
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, f := range b.files {
		hdr := &tar.Header{Name: "cucina-support/" + f.name, Mode: 0o644, Size: int64(len(f.data)), ModTime: now, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// stripObjectMeta drops managed fields and the last-applied annotation (a copy of
// the whole object) from a resource before it enters the bundle.
func stripObjectMeta(annotations map[string]string) map[string]string {
	if _, ok := annotations[lastAppliedConfigKey]; !ok {
		return annotations
	}
	out := make(map[string]string, len(annotations))
	for k, v := range annotations {
		if k != lastAppliedConfigKey {
			out[k] = v
		}
	}
	return out
}

// collectBundle gathers the bundle content. Sources that fail are listed in
// errors.json; the bundle is still produced.
func (s *Server) collectBundle(ctx context.Context, includeLogs bool) *bundle {
	b := &bundle{}
	now := time.Now()
	version := map[string]any{
		"version":     s.opts.Version,
		"clusterId":   s.opts.ClusterID,
		"protocol":    cproto.Current.String(),
		"goVersion":   runtime.Version(),
		"collectedAt": now.UTC().Format(time.RFC3339),
		"collectedBy": caller(ctx),
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		deps := map[string]string{}
		for _, d := range bi.Deps {
			deps[d.Path] = d.Version
		}
		version["modules"] = deps
	}
	b.addJSON("version.json", version)
	if s.opts.Config != nil {
		b.addJSON("config/controller.json", s.opts.Config)
	}

	if pools, err := s.deps.Pools.Pools(ctx); err != nil {
		b.fail("resources/workerpools.json", err)
	} else {
		items := make([]v1alpha1.WorkerPool, 0, len(pools))
		specs := make([]any, 0, len(pools))
		for _, p := range pools {
			r := *p.Resource.DeepCopy()
			r.ManagedFields = nil
			r.Annotations = stripObjectMeta(r.Annotations)
			items = append(items, r)
			specs = append(specs, map[string]any{"name": p.Name(), "resolved": p.Spec, "floor": p.Floor})
		}
		b.addJSON("resources/workerpools.json", items)
		b.addJSON("state/pools-resolved.json", specs)
	}
	if s.deps.Hosts != nil {
		if hosts, err := s.deps.Hosts.Hosts(ctx); err != nil {
			b.fail("resources/machosts.json", err)
		} else {
			items := make([]v1alpha1.MacHost, 0, len(hosts))
			for _, h := range hosts {
				c := h.DeepCopy()
				c.ManagedFields = nil
				c.Annotations = stripObjectMeta(c.Annotations)
				items = append(items, *c)
			}
			b.addJSON("resources/machosts.json", items)
		}
	}
	if s.deps.Support != nil {
		if tps, err := s.deps.Support.TrustPolicies(ctx); err != nil {
			b.fail("resources/trustpolicies.json", err)
		} else {
			items := make([]v1alpha1.TrustPolicy, 0, len(tps))
			for _, tp := range tps {
				c := tp.DeepCopy()
				c.ManagedFields = nil
				c.Annotations = stripObjectMeta(c.Annotations)
				items = append(items, *c)
			}
			b.addJSON("resources/trustpolicies.json", items)
		}
		if m, err := s.deps.Support.Metrics(ctx); err != nil {
			b.fail("metrics/controller.prom", err)
		} else {
			b.addText("metrics/controller.prom", m)
		}
	}

	// The same views the CLI shows, from the RPC implementations.
	type dump struct {
		name string
		get  func() (proto.Message, error)
	}
	dumps := []dump{
		{"state/status.json", func() (proto.Message, error) { return s.GetStatus(ctx, &cucinav1.GetStatusRequest{}) }},
		{"state/pools.json", func() (proto.Message, error) { return s.ListPools(ctx, &cucinav1.ListPoolsRequest{}) }},
		{"state/workers.json", func() (proto.Message, error) { return s.ListWorkers(ctx, &cucinav1.ListWorkersRequest{}) }},
		{"state/hosts.json", func() (proto.Message, error) { return s.ListHosts(ctx, &cucinav1.ListHostsRequest{}) }},
		{"state/queues.json", func() (proto.Message, error) { return s.ListQueues(ctx, &cucinav1.ListQueuesRequest{}) }},
		{"state/operations.json", func() (proto.Message, error) {
			return s.ListOperations(ctx, &cucinav1.ListOperationsRequest{Page: &cucinav1.Page{Size: bundleMaxOperations}})
		}},
	}
	optional := []struct {
		present bool
		d       dump
	}{
		{s.deps.Images != nil, dump{"state/images.json", func() (proto.Message, error) { return s.ListImages(ctx, &cucinav1.ListImagesRequest{}) }}},
		{s.deps.Cost != nil, dump{"state/cost.json", func() (proto.Message, error) { return s.GetCost(ctx, &cucinav1.GetCostRequest{}) }}},
		{s.deps.Enrollment != nil, dump{"state/enroll-tokens.json", func() (proto.Message, error) {
			return s.ListEnrollTokens(ctx, &cucinav1.ListEnrollTokensRequest{})
		}}},
		{s.deps.Keys != nil, dump{"state/service-keys.json", func() (proto.Message, error) {
			return s.ListServiceKeys(ctx, &cucinav1.ListServiceKeysRequest{})
		}}},
		{s.deps.Revocations != nil, dump{"state/revocations.json", func() (proto.Message, error) {
			return s.ListRevocations(ctx, &cucinav1.ListRevocationsRequest{})
		}}},
	}
	for _, o := range optional {
		if o.present {
			dumps = append(dumps, o.d)
		}
	}
	for _, d := range dumps {
		m, err := d.get()
		if err != nil {
			b.fail(d.name, err)
			continue
		}
		b.addJSON(d.name, m)
	}
	if includeLogs && s.deps.LogTail != nil {
		b.addText("logs/controller.log", s.deps.LogTail.Tail(bundleLogLines))
	}
	return b
}

// CollectSupportBundle streams a tar.gz with version information, the redacted
// controller configuration, the Cucina resources, queue/host/pool/worker dumps, a
// metrics snapshot and (include_logs) the controller log tail. One bundle is
// collected at a time.
func (s *Server) CollectSupportBundle(req *cucinav1.CollectSupportBundleRequest, stream grpc.ServerStreamingServer[cucinav1.LogChunk]) error {
	release, err := s.bundles.acquire("support bundles")
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(stream.Context(), bundleTimeout)
	defer cancel()
	b := s.collectBundle(ctx, req.GetIncludeLogs())
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(b.write(pw, time.Now())) }()
	defer func() { _ = pr.Close() }()
	return s.sendChunks(ctx, stream, pr)
}

// ---------------------------------------------------------------- log ring

// LogRing keeps the most recent bytes written to it (the controller's own JSON log)
// for support bundles. Wire it next to stdout: slog.NewJSONHandler(io.MultiWriter(os.Stdout, ring), …).
// It holds between maxBytes and 2×maxBytes; trimming is amortized (one copy per
// maxBytes written).
type LogRing struct {
	mu  sync.Mutex
	buf []byte
	max int
}

// NewLogRing returns a ring that keeps at least the last maxBytes (at most 2×maxBytes).
func NewLogRing(maxBytes int) *LogRing {
	return &LogRing{max: max(maxBytes, 4096)}
}

// Write implements io.Writer; it never fails.
func (r *LogRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > 2*r.max {
		keep := r.buf[len(r.buf)-r.max:]
		if i := bytes.IndexByte(keep, '\n'); i >= 0 && i < len(keep)-1 {
			keep = keep[i+1:] // start at a line boundary
		}
		n := copy(r.buf, keep)
		r.buf = r.buf[:n]
	}
	return len(p), nil
}

// Tail returns the last n complete lines.
func (r *LogRing) Tail(n int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(lastLines(r.buf, n))
}
