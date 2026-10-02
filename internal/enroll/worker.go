// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/ports"
)

// ErrReplay is returned when an instance launch already used its enrollment.
var ErrReplay = errors.New("enrollment replay")

// launchTimeTolerance bounds |DescribeInstances LaunchTime - document pendingTime|.
const launchTimeTolerance = 2 * time.Minute

// notVisibleRetry is the retry hint for instances DescribeInstances does not show yet.
const notVisibleRetry = 5 * time.Second

// retryLater is a retryable Unavailable status with a RetryInfo hint.
func retryLater(msg string, after time.Duration) error {
	st := status.New(codes.Unavailable, msg)
	if withHint, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(after)}); err == nil {
		st = withHint
	}
	return st.Err()
}

// EnrollWorker implements cucinav1.EnrollmentServiceServer. It is called by
// `cucina-worker-agent bootstrap` at every boot of an EC2 worker (R-POOL-3) and
// issues a worker certificate only if, in order: the identity document's
// RSA-2048 signature verifies with the AWS certificate of its Region; account,
// Region and pendingTime freshness match; a tag-filtered Describe shows the
// instance pending/running with this cluster's tags; the controller's launch
// records know the launch; the pool is configured; the CSR is acceptable; and
// this launch has not enrolled before (beyond a short same-key crash-recovery
// window). Everything else fails closed (R-SEC-3).
func (s *Server) EnrollWorker(ctx context.Context, req *cucinav1.EnrollWorkerRequest) (*cucinav1.EnrollWorkerResponse, error) {
	const event = "enroll.worker"
	if !s.workerRate.allow(ctx) {
		s.audit(ctx, event, "rate-limited")
		return nil, status.Error(codes.ResourceExhausted, "too many enrollment requests from this address")
	}
	if err := checkProtocol(req.GetProtocol(), "cucina-worker-agent"); err != nil {
		s.audit(ctx, event, "protocol-mismatch")
		return nil, err
	}
	if s.d.Compute == nil {
		return nil, status.Error(codes.FailedPrecondition, "this Cucina installation has no EC2 pools")
	}
	doc, err := s.d.Identity.Verify(req.GetInstanceIdentityDocument(), req.GetSignature())
	if err != nil {
		s.audit(ctx, event, "unauthenticated", "reason", err.Error())
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	deny := func(reason string) error {
		s.audit(ctx, event, "denied", "instance", doc.InstanceID, "reason", reason)
		return status.Error(codes.PermissionDenied, "enrollment refused: "+reason)
	}
	now := s.d.Clock.Now()
	switch age := now.Sub(doc.PendingTime); {
	case doc.AccountID != s.o.AWSAccountID:
		return nil, deny("the identity document belongs to another AWS account")
	case doc.Region != s.o.AWSRegion:
		return nil, deny(fmt.Sprintf("the identity document is from Region %q, not %q", doc.Region, s.o.AWSRegion))
	case !strings.HasPrefix(doc.AvailabilityZone, doc.Region):
		return nil, deny("the availability zone does not belong to the Region")
	case doc.PendingTime.IsZero():
		return nil, deny("the identity document has no pendingTime")
	case age < -s.o.ClockSkew:
		return nil, deny("pendingTime is in the future")
	case age > s.o.MaxBootAge:
		return nil, deny(fmt.Sprintf("the identity document is stale (launched %s ago, limit %s)", age.Round(time.Second), s.o.MaxBootAge))
	case req.GetArch() != "" && req.GetArch() != doc.Architecture:
		return nil, deny("the reported architecture differs from the signed identity document")
	}

	// Every state, so that a terminated instance is a definitive refusal while
	// one that DescribeInstances does not show yet (EC2 eventual consistency
	// right after RunInstances; the document is fresh) is retryable.
	insts, err := s.d.Compute.Describe(ctx, ports.InstanceFilter{
		Cluster: s.o.ClusterID, IDs: []string{doc.InstanceID},
		States: []ports.InstanceState{ports.InstancePending, ports.InstanceRunning, ports.InstanceShuttingDown,
			ports.InstanceTerminated, ports.InstanceStopping, ports.InstanceStopped},
	})
	if err != nil {
		return nil, s.unavailable(ctx, "describing the instance", err)
	}
	var inst *ports.Instance
	for i := range insts {
		if insts[i].ID == doc.InstanceID {
			inst = &insts[i]
		}
	}
	if inst == nil {
		s.audit(ctx, event, "retry", "instance", doc.InstanceID, "reason", "not visible in DescribeInstances yet")
		return nil, retryLater(fmt.Sprintf("instance %s is not visible as an instance of this Cucina cluster yet (EC2 eventual consistency); retry", doc.InstanceID), notVisibleRetry)
	}
	if reason := checkInstance(*inst, s.o.ClusterID, doc); reason != "" {
		return nil, deny(reason)
	}
	if err := s.d.Launches.VerifyLaunch(ctx, *inst); err != nil {
		if errors.Is(err, ErrUnknownLaunch) {
			return nil, deny("the instance was not launched by this controller")
		}
		return nil, s.unavailable(ctx, "checking launch records", err)
	}
	pool := inst.Tags[domain.TagPool]
	generation := inst.Tags[domain.TagGeneration]
	id, err := pki.WorkerIdentity(pool, doc.InstanceID)
	if err != nil {
		return nil, deny(fmt.Sprintf("pool %q cannot form a worker identity", pool))
	}
	settings, _, err := s.d.Pools.SettingsFor(pool, doc.InstanceID)
	if err != nil {
		s.audit(ctx, event, "denied", "instance", doc.InstanceID, "pool", pool, "reason", "pool not configured")
		return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("pool %q is not configured on this controller", pool))
	}
	pub, err := pki.ParseCSR(req.GetCsrPem(), id)
	if err != nil {
		s.audit(ctx, event, "bad-request", "instance", doc.InstanceID, "reason", err.Error())
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	keyHash, err := pki.PublicKeyHash(pub)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// One enrollment per launch; the same key may retry within the replay
	// window (agent crashed after the response), a different key never.
	err = s.d.Replay.Claim(ctx, doc.InstanceID, func(rec *LaunchRecord, found bool) error {
		if !found || doc.PendingTime.After(rec.PendingTime) {
			*rec = LaunchRecord{InstanceID: doc.InstanceID, PendingTime: doc.PendingTime, KeySHA256: keyHash, FirstIssued: now, Issued: 1}
			return nil
		}
		switch {
		case !doc.PendingTime.Equal(rec.PendingTime):
			return fmt.Errorf("%w: the identity document predates this instance's last enrollment", ErrReplay)
		case rec.KeySHA256 != keyHash:
			return fmt.Errorf("%w: this instance launch already enrolled with another key", ErrReplay)
		case now.Sub(rec.FirstIssued) > s.o.ReplayWindow:
			return fmt.Errorf("%w: the enrollment window of this instance launch has closed (a rebooted worker is replaced, not re-admitted)", ErrReplay)
		case rec.Issued >= s.o.MaxIssuancesPerLaunch:
			return fmt.Errorf("%w: too many enrollments for this instance launch", ErrReplay)
		}
		rec.Issued++
		return nil
	})
	if errors.Is(err, ErrReplay) {
		return nil, deny(err.Error())
	}
	if err != nil {
		return nil, s.unavailable(ctx, "recording the enrollment", err)
	}

	issued, err := s.d.Issuer.IssueWorker(req.GetCsrPem(), pool, doc.InstanceID)
	if err != nil {
		if errors.Is(err, pki.ErrBadCSR) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, s.unavailable(ctx, "issuing the certificate", err)
	}
	s.audit(ctx, event, "issued", "instance", doc.InstanceID, "pool", pool, "generation", generation,
		"identity", issued.Identity.String(), "notAfter", issued.NotAfter.UTC().Format(time.RFC3339))
	return &cucinav1.EnrollWorkerResponse{
		CertificatePem: issued.ChainPEM,
		CaPem:          s.bundle(issued),
		ExpiresAt:      timestamppb.New(issued.NotAfter),
		Pool:           pool,
		Generation:     generation,
		Settings:       settings,
	}, nil
}

// checkInstance cross-checks the Describe result against the cluster's tags and
// the signed document; it returns a denial reason or "".
func checkInstance(inst ports.Instance, cluster string, doc *IdentityDocument) string {
	t := inst.Tags
	switch {
	case inst.State != ports.InstancePending && inst.State != ports.InstanceRunning:
		return fmt.Sprintf("the instance is %s", inst.State)
	case t[domain.TagManagedBy] != domain.ManagedByValue:
		return "the instance is not managed by cucina-controller"
	case t[domain.TagCluster] != cluster:
		return "the instance belongs to another Cucina cluster"
	case t[domain.TagPool] == "":
		return "the instance has no pool tag"
	case inst.Pool != "" && string(inst.Pool) != t[domain.TagPool]:
		return "the instance's pool is inconsistent"
	case t[domain.TagGeneration] == "":
		return "the instance has no generation tag"
	case t[domain.TagLaunchToken] == "":
		return "the instance has no launch-token tag"
	case t[domain.TagRole] != "" && t[domain.TagRole] != "worker":
		return "the instance is not a worker"
	case inst.ImageID != doc.ImageID:
		return "the instance's image differs from the signed identity document"
	case inst.AZ != doc.AvailabilityZone:
		return "the instance's availability zone differs from the signed identity document"
	case inst.LaunchTime.IsZero() || inst.LaunchTime.Sub(doc.PendingTime).Abs() > launchTimeTolerance:
		return "the instance's launch time differs from the signed identity document"
	}
	return ""
}
