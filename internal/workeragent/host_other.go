// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux && !darwin && !windows

package workeragent

import (
	"context"
	"errors"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// NewHost returns a Host that reports the OS as unsupported.
func NewHost(ports.FS, string) Host { return otherHost{} }

type otherHost struct{}

var errUnsupportedOS = errors.New("unsupported operating system")

func (otherHost) BootID() (string, error)        { return "", errUnsupportedOS }
func (otherHost) Uptime() (time.Duration, error) { return 0, errUnsupportedOS }
func (otherHost) MemoryBytes() (uint64, error)   { return 0, errUnsupportedOS }

func protectKey(string, []string) error { return nil }

func systemShuttingDown(context.Context, ports.Exec) bool { return false }
