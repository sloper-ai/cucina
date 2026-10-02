// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"errors"
	"fmt"
)

// Code is the RFC 6749 error code an exchange failure maps to (contracts §5.1).
type Code string

const (
	// CodeInvalidRequest: the request is malformed.
	CodeInvalidRequest Code = "invalid_request"
	// CodeInvalidGrant: the subject token is invalid (malformed, signature, expired,
	// untrusted issuer, wrong audience, replayed, unknown or revoked service key).
	CodeInvalidGrant Code = "invalid_grant"
	// CodeAccessDenied: the subject token is valid but no trust policy grants anything.
	CodeAccessDenied Code = "access_denied"
	// CodeServerError: Cucina failed (fail closed).
	CodeServerError Code = "server_error"
)

// Error is an exchange failure. Reason is safe to return to the client and to audit:
// it never contains token material. Policy names the deciding TrustPolicy when known.
type Error struct {
	Code   Code
	Reason string
	Policy string
	// Issuer is the bounded issuer label (a configured issuer URL, "service-account" or
	// "unknown"), used for metrics.
	Issuer string
	cause  error
}

func (e *Error) Error() string {
	if e.Policy != "" {
		return fmt.Sprintf("%s: %s (policy %s)", e.Code, e.Reason, e.Policy)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Reason)
}

// Unwrap exposes the internal cause (never sent to clients).
func (e *Error) Unwrap() error { return e.cause }

func errf(code Code, issuer, format string, args ...any) *Error {
	return &Error{Code: code, Issuer: issuer, Reason: fmt.Sprintf(format, args...)}
}

// AsError converts any error into an *Error (unknown errors become server_error).
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeServerError, Reason: "internal error", Issuer: IssuerUnknown, cause: err}
}

// Issuer labels that are not issuer URLs.
const (
	IssuerUnknown        = "unknown"
	IssuerServiceAccount = "service-account"
)

// VerifyError is returned by token verification with a coarse, client-safe category.
type VerifyError struct {
	Category string // malformed, signature, expired, not-yet-valid, issuer, audience, unavailable
	cause    error
}

func (e *VerifyError) Error() string { return "subject token verification failed: " + e.Category }

// Unwrap returns the underlying library error (may include claim values; never log it
// at a level that reaches clients).
func (e *VerifyError) Unwrap() error { return e.cause }

func verifyErr(category string, cause error) error {
	return &VerifyError{Category: category, cause: cause}
}
