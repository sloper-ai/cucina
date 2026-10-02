// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"log/slog"
)

// TracingSetup is the optional OTLP tracing hook (R-OBS-4). The default build
// has no tracer: a configured endpoint is reported and ignored. A
// components_otel.go file may replace it with an OpenTelemetry exporter
// (go.opentelemetry.io/otel + otelgrpc, R-LIB-2) without touching anything else.
var TracingSetup = func(ctx context.Context, endpoint string, log *slog.Logger) (shutdown func(context.Context) error, err error) {
	if endpoint != "" {
		log.Warn("OTLP tracing requested but no exporter is compiled in; continuing without tracing", "endpoint", endpoint)
	}
	return func(context.Context) error { return nil }, nil
}

// SetupTracing calls TracingSetup.
func SetupTracing(ctx context.Context, endpoint string, log *slog.Logger) (func(context.Context) error, error) {
	return TracingSetup(ctx, endpoint, log)
}
