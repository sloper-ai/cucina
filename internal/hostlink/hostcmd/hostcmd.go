// SPDX-License-Identifier: FSL-1.1-ALv2

// Package hostcmd defines the machine-readable error codes hostd puts in
// CommandResult.error ("<code>: <message>") so the controller maps rejections
// to ports sentinels (HostFleet contract) without parsing prose.
package hostcmd

import (
	"errors"
	"strings"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Error codes.
const (
	CodeVMLimit  = "vm-limit"  // no free VM slot (2-VM Apple limit or host slots)
	CodeCordoned = "cordoned"  // host is cordoned
	CodeNotFound = "not-found" // unknown VM
	CodeInvalid  = "invalid"   // malformed command
	CodeFailed   = "failed"    // execution failed
)

// ErrCordoned is returned for commands refused by a cordoned host.
var ErrCordoned = errors.New("host is cordoned")

// Encode renders a coded error message.
func Encode(code, msg string) string { return code + ": " + msg }

// Decode maps a CommandResult.error to a sentinel-wrapping error.
func Decode(s string) error {
	code, msg, ok := strings.Cut(s, ": ")
	if !ok {
		return errors.New(s)
	}
	switch code {
	case CodeVMLimit:
		return wrap(ports.ErrVMLimit, msg)
	case CodeCordoned:
		return wrap(ErrCordoned, msg)
	case CodeNotFound:
		return wrap(ports.ErrVMNotFound, msg)
	case CodeInvalid:
		return wrap(ports.ErrInvalid, msg)
	}
	return errors.New(s)
}

func wrap(sentinel error, msg string) error { return &codedError{sentinel: sentinel, msg: msg} }

type codedError struct {
	sentinel error
	msg      string
}

func (e *codedError) Error() string { return e.sentinel.Error() + ": " + e.msg }
func (e *codedError) Unwrap() error { return e.sentinel }
