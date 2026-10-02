// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/sloper-ai/cucina/internal/ports"
)

// errKind classifies an EC2/Pricing API error. The controller's retry and
// backoff logic only ever sees the ports.Err* sentinels the kinds map to.
type errKind int

const (
	kindUnknown            errKind = iota // transient or unclassified (5xx, network): returned unwrapped
	kindCapacity                          // no capacity for this (type, subnet): try the next combination
	kindQuota                             // vCPU/instance/spot quota: try the next combination
	kindThrottled                         // API rate limit: stop, the caller backs off
	kindImage                             // AMI missing or unavailable
	kindNotFound                          // the addressed resource does not exist
	kindInvalid                           // permission or validation error: permanent
	kindIdempotentMismatch                // client token already used with other parameters
	kindTokenTerminated                   // client token belongs to a terminated instance
)

var (
	capacityCodes = codeSet(
		"InsufficientInstanceCapacity", "InsufficientCapacity", "InsufficientHostCapacity",
		"InsufficientReservedInstanceCapacity", "InsufficientCapacityOnOutpost",
		"InsufficientFreeAddressesInSubnet", "InsufficientVolumeCapacity", "UnfulfillableCapacity",
		"SpotMaxPriceTooLow",
	)
	quotaCodes = codeSet(
		"VcpuLimitExceeded", "InstanceLimitExceeded", "MaxSpotInstanceCountExceeded",
		"VolumeLimitExceeded", "MaxIOPSLimitExceeded", "NetworkInterfaceLimitExceeded",
		"ResourceLimitExceeded", "AddressLimitExceeded",
	)
	throttleCodes = codeSet(
		"RequestLimitExceeded", "Throttling", "ThrottlingException", "ThrottledException",
		"RequestThrottled", "RequestThrottledException", "TooManyRequestsException", "SlowDown",
		"EC2ThrottledException", "PriorRequestNotComplete", "BandwidthLimitExceeded",
		"LimitExceededException",
	)
	invalidCodes = codeSet(
		"UnauthorizedOperation", "AuthFailure", "Blocked", "OptInRequired", "AccessDenied",
		"AccessDeniedException", "MissingParameter", "UnknownParameter", "ValidationError",
		"ValidationException", "UnsupportedOperation", "OperationNotPermitted", "IncorrectState",
		"IncorrectInstanceState", "SignatureDoesNotMatch", "InvalidClientTokenId", "DryRunOperation",
		"Unsupported", "VolumeInUse",
	)
)

func codeSet(codes ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(codes))
	for _, c := range codes {
		m[c] = struct{}{}
	}
	return m
}

// errorCode returns the AWS error code of err ("" if err is not an API error).
func errorCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// classify maps an API error to its kind. Context-specific refinements (for
// example "Unsupported" during a launch walk) are applied by the callers.
func classify(err error) errKind {
	if err == nil {
		return kindUnknown
	}
	var quota ratelimit.QuotaExceededError
	if errors.As(err, &quota) {
		return kindThrottled // the SDK's client-side retry quota is exhausted
	}
	code := errorCode(err)
	if _, ok := throttleCodes[code]; ok {
		return kindThrottled
	}
	if _, ok := capacityCodes[code]; ok {
		return kindCapacity
	}
	if _, ok := quotaCodes[code]; ok {
		return kindQuota
	}
	switch {
	case code == "IdempotentParameterMismatch":
		return kindIdempotentMismatch
	case code == "IdempotentInstanceTerminated":
		return kindTokenTerminated
	case strings.HasPrefix(code, "InvalidAMIID.") || code == "InvalidAMIName.NotFound":
		return kindImage
	case strings.HasSuffix(code, ".NotFound") || strings.HasSuffix(code, ".NotFoundException"):
		return kindNotFound
	case strings.HasPrefix(code, "Invalid"):
		return kindInvalid
	}
	if _, ok := invalidCodes[code]; ok {
		return kindInvalid
	}
	return kindUnknown
}

// sentinel returns the ports sentinel for kind (nil for kindUnknown).
func sentinel(kind errKind) error {
	switch kind {
	case kindCapacity:
		return ports.ErrInsufficientCapacity
	case kindQuota:
		return ports.ErrQuotaExceeded
	case kindThrottled:
		return ports.ErrThrottled
	case kindImage:
		return ports.ErrImageNotFound
	case kindNotFound:
		return ports.ErrNotFound
	case kindInvalid, kindIdempotentMismatch, kindTokenTerminated:
		return ports.ErrInvalid
	}
	return nil
}

// wrapKind wraps err (from operation op) with the sentinel of kind.
func wrapKind(op string, kind errKind, err error) error {
	if kind == kindThrottled {
		return &ThrottleError{Op: op, Code: errorCode(err), RetryAfter: retryAfter(err), Err: err}
	}
	if s := sentinel(kind); s != nil {
		return fmt.Errorf("ec2 %s: %w: %w", op, s, err)
	}
	return fmt.Errorf("ec2 %s: %w", op, err)
}

// isContextErr reports whether err stems from a cancelled or expired context.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// retryAfter extracts a Retry-After header (seconds or HTTP date) from err.
func retryAfter(err error) time.Duration {
	var re *smithyhttp.ResponseError
	if !errors.As(err, &re) || re.Response == nil || re.Response.Response == nil {
		return 0
	}
	v := strings.TrimSpace(re.Response.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := time.Parse(time.RFC1123, v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Attempt is one (instance type, subnet, capacity type) combination a Launch tried.
type Attempt struct {
	InstanceType string
	SubnetID     string
	CapacityType ports.CapacityType
	// Code is the EC2 error code the attempt failed with (e.g. InsufficientInstanceCapacity).
	Code string
}

// CapacityError reports that every (type, subnet) combination of a Launch failed with a
// capacity or quota error. It returns fast — the caller owns backoff (R-SCALE-4) — and
// lists what was tried. errors.Is matches ports.ErrInsufficientCapacity when at least one
// attempt hit a capacity error and ports.ErrQuotaExceeded when at least one hit a quota.
type CapacityError struct {
	Tried []Attempt
}

func (e *CapacityError) Error() string {
	var b strings.Builder
	b.WriteString("ec2 launch: no capacity in any of ")
	b.WriteString(strconv.Itoa(len(e.Tried)))
	b.WriteString(" combination(s):")
	for _, a := range e.Tried {
		fmt.Fprintf(&b, " %s/%s/%s=%s", a.InstanceType, a.SubnetID, a.CapacityType, a.Code)
	}
	return b.String()
}

// Unwrap returns the sentinels this error matches.
func (e *CapacityError) Unwrap() []error {
	var capacity, quota bool
	for _, a := range e.Tried {
		if _, ok := quotaCodes[a.Code]; ok {
			quota = true
		} else {
			capacity = true
		}
	}
	var out []error
	if capacity || len(e.Tried) == 0 {
		out = append(out, ports.ErrInsufficientCapacity)
	}
	if quota {
		out = append(out, ports.ErrQuotaExceeded)
	}
	return out
}

// ThrottleError is returned when EC2 (or the Price List API) still throttles after the
// SDK's adaptive retries. It matches ports.ErrThrottled; RetryAfter is the server's
// Retry-After hint (0 if none).
type ThrottleError struct {
	Op         string
	Code       string
	RetryAfter time.Duration
	Err        error
}

func (e *ThrottleError) Error() string {
	s := "ec2 " + e.Op + ": " + ports.ErrThrottled.Error()
	if e.Code != "" {
		s += " (" + e.Code + ")"
	}
	if e.RetryAfter > 0 {
		s += ", retry after " + e.RetryAfter.String()
	}
	return s
}

// Unwrap returns ports.ErrThrottled and the underlying API error.
func (e *ThrottleError) Unwrap() []error { return []error{ports.ErrThrottled, e.Err} }
