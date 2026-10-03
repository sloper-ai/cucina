// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/internal/pki"
)

// CAAdmin applies one explicit operator-driven phase to the configured CA. It
// does not distribute trust, restart verifiers, retire leaves or change MDM.
type CAAdmin interface {
	RotateCA(ctx context.Context, phase pki.RotationPhase) error
}

// CAAdapter exposes the existing optimistic CA Secret operation. Client must
// read directly from the API server, not from the controller's informer cache.
// Conflicts are returned to the operator, never retried across a phase boundary.
type CAAdapter struct {
	Client     client.Client
	Namespace  string
	SecretName string
}

var _ CAAdmin = (*CAAdapter)(nil)

// RotateCA implements CAAdmin. Error messages deliberately contain no Secret
// content, certificate material or upstream error text (the guard audits them).
func (a *CAAdapter) RotateCA(ctx context.Context, phase pki.RotationPhase) error {
	if a.Client == nil || a.Namespace == "" || a.SecretName == "" {
		return notConfigured("CA rotation")
	}
	err := pki.RotateCA(ctx, a.Client, a.Namespace, a.SecretName, phase)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "CA rotation canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "CA rotation deadline exceeded")
	case apierrors.IsConflict(err):
		return status.Error(codes.Aborted, "CA Secret changed concurrently; inspect the current rotation state before retrying")
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return status.Error(codes.PermissionDenied, "controller is not allowed to update the configured CA Secret")
	case apierrors.IsNotFound(err):
		return status.Error(codes.FailedPrecondition, "configured CA Secret does not exist")
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err):
		return status.Error(codes.Unavailable, "CA Secret is temporarily unavailable")
	default:
		return status.Error(codes.FailedPrecondition, "CA phase could not be applied; verify the current rotation state and Cucina-managed CA configuration")
	}
}
