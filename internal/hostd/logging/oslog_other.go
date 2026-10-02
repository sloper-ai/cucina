// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !darwin || !cgo

package logging

import "log/slog"

// newOSLogHandler is unavailable without darwin+cgo (unified logging).
func newOSLogHandler(string, *slog.HandlerOptions) slog.Handler { return nil }
