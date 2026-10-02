// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package logging

/*
#include <os/log.h>
#include <stdlib.h>

static os_log_t cucina_new_log(const char *subsystem, const char *category) {
	return os_log_create(subsystem, category);
}

static void cucina_log(os_log_t log, int level, const char *msg) {
	os_log_type_t t = OS_LOG_TYPE_DEFAULT;
	switch (level) {
	case 0: t = OS_LOG_TYPE_DEBUG; break;
	case 1: t = OS_LOG_TYPE_INFO; break;
	case 2: t = OS_LOG_TYPE_DEFAULT; break;
	default: t = OS_LOG_TYPE_ERROR; break;
	}
	os_log_with_type(log, t, "%{public}s", msg);
}
*/
import "C"

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"unsafe"
)

// osLogHandler renders each record as JSON and writes it to unified logging
// (subsystem ai.sloper.cucina) at the matching os_log type.
type osLogHandler struct {
	log  C.os_log_t
	mu   *sync.Mutex
	buf  *bytes.Buffer
	json slog.Handler
}

func newOSLogHandler(category string, opts *slog.HandlerOptions) slog.Handler {
	cs := C.CString(Subsystem)
	defer C.free(unsafe.Pointer(cs))
	cc := C.CString(category)
	defer C.free(unsafe.Pointer(cc))
	buf := &bytes.Buffer{}
	return &osLogHandler{log: C.cucina_new_log(cs, cc), mu: &sync.Mutex{}, buf: buf, json: slog.NewJSONHandler(buf, opts)}
}

func (h *osLogHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.json.Enabled(ctx, l) }

func (h *osLogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.Reset()
	if err := h.json.Handle(ctx, r); err != nil {
		return err
	}
	msg := C.CString(string(bytes.TrimRight(h.buf.Bytes(), "\n")))
	defer C.free(unsafe.Pointer(msg))
	level := 1
	switch {
	case r.Level < slog.LevelInfo:
		level = 0
	case r.Level < slog.LevelWarn:
		level = 1
	case r.Level < slog.LevelError:
		level = 2
	default:
		level = 3
	}
	C.cucina_log(h.log, C.int(level), msg)
	return nil
}

func (h *osLogHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &osLogHandler{log: h.log, mu: h.mu, buf: h.buf, json: h.json.WithAttrs(a)}
}

func (h *osLogHandler) WithGroup(n string) slog.Handler {
	return &osLogHandler{log: h.log, mu: h.mu, buf: h.buf, json: h.json.WithGroup(n)}
}
