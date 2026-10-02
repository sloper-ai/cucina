// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// AuditEvent is one audit record: a call to a mutating or sensitive method
// (R-CLI-2, R-AUTH-11). Request is a redacted JSON summary of the request message;
// responses (which may carry a key or token shown once) are never recorded.
type AuditEvent struct {
	Time        time.Time
	Principal   string // JWT sub; empty when authentication failed
	DisplayName string
	SessionID   string
	Method      string // full gRPC method name
	Mutating    bool
	Request     json.RawMessage
	Code        string // gRPC status code name, "OK" on success
	Error       string // redacted status message on failure
	Duration    time.Duration
	Peer        string
}

// Auditor receives audit records. Implementations must not block for long: the
// record is written before the response is returned.
type Auditor interface {
	Record(ctx context.Context, e AuditEvent)
}

// SlogAuditor writes one structured JSON line per record through a slog logger
// (use a slog.JSONHandler). Lines carry `"log":"audit"` so log pipelines can route
// them.
type SlogAuditor struct {
	logger *slog.Logger
}

// NewSlogAuditor returns an Auditor writing to logger.
func NewSlogAuditor(logger *slog.Logger) *SlogAuditor {
	return &SlogAuditor{logger: logger}
}

// Record implements Auditor.
func (a *SlogAuditor) Record(ctx context.Context, e AuditEvent) {
	ctx = context.WithoutCancel(ctx)
	h := a.logger.Handler()
	if !h.Enabled(ctx, slog.LevelInfo) {
		return
	}
	attrs := []slog.Attr{
		slog.String("log", "audit"),
		slog.String("principal", e.Principal),
		slog.String("display_name", e.DisplayName),
		slog.String("session", e.SessionID),
		slog.String("method", e.Method),
		slog.Bool("mutating", e.Mutating),
		slog.Any("request", e.Request),
		slog.String("code", e.Code),
		slog.Float64("duration_seconds", e.Duration.Seconds()),
		slog.String("peer", e.Peer),
	}
	if e.Error != "" {
		attrs = append(attrs, slog.String("error", e.Error))
	}
	r := slog.NewRecord(e.Time, slog.LevelInfo, "audit", 0)
	r.AddAttrs(attrs...)
	_ = h.Handle(ctx, r)
}

// maxAuditRequestBytes bounds the request summary in an audit record.
const maxAuditRequestBytes = 4 << 10

// audit records one call.
func (s *Server) audit(ctx context.Context, p Principal, method string, a Access, req proto.Message, err error, d time.Duration) {
	e := AuditEvent{
		Time:        time.Now(),
		Principal:   p.Subject,
		DisplayName: p.DisplayName,
		SessionID:   p.SessionID,
		Method:      method,
		Mutating:    a.Mutating(),
		Request:     summarizeRequest(req),
		Code:        status.Code(err).String(),
		Duration:    d,
	}
	if err != nil {
		e.Error = RedactText(status.Convert(err).Message())
	}
	if pr, ok := peer.FromContext(ctx); ok && pr.Addr != nil {
		e.Peer = pr.Addr.String()
	}
	s.deps.Auditor.Record(ctx, e)
}

// summarizeRequest renders a request message as redacted, size-bounded JSON.
func summarizeRequest(m proto.Message) json.RawMessage {
	if m == nil {
		return json.RawMessage(`{}`)
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return json.RawMessage(`{"unrenderable":true}`)
	}
	out, err := RedactJSON(b)
	if err != nil {
		return json.RawMessage(`{"unrenderable":true}`)
	}
	if len(out) > maxAuditRequestBytes {
		trunc, _ := json.Marshal(map[string]any{"truncated": true, "bytes": len(out)})
		return trunc
	}
	return out
}
