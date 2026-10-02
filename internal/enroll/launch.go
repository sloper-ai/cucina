// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"context"
	"errors"
	"fmt"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// LaunchRecords confirms that the controller itself launched inst (its
// cucina:launch-token matches the controller's launch records for the
// instance's pool). EnrollWorker calls it after the tag-filtered Describe; an
// ErrUnknownLaunch refuses enrollment, any other error is retryable (fail closed).
type LaunchRecords interface {
	VerifyLaunch(ctx context.Context, inst ports.Instance) error
}

// ErrUnknownLaunch is returned for instances the controller did not launch.
var ErrUnknownLaunch = errors.New("instance was not launched by this controller")

// LedgerLaunches implements LaunchRecords over the autoscaler's launch-token
// ledger (internal/scaling): every EC2 launch carries the token
// "<TokenPrefix(cluster, pool)>-<epoch>-<seq>", and the ledger (persisted on the
// WorkerPool before any launch, write-ahead) records the pool's epoch and the
// next unused seq. A token is the controller's own if its prefix is the
// instance's pool's, its epoch is the pool's current epoch and its seq was
// allocated. Instances of a lost/reset ledger fail (they are replaced).
type LedgerLaunches struct {
	Cluster string
	// Ledger returns the persisted ledger of a pool (reconcile.ReadLedger of
	// the WorkerPool); an error is treated as transient.
	Ledger func(ctx context.Context, pool domain.PoolName) (scaling.Ledger, error)
}

// VerifyLaunch implements LaunchRecords.
func (l LedgerLaunches) VerifyLaunch(ctx context.Context, inst ports.Instance) error {
	pool := domain.PoolName(inst.Tags[domain.TagPool])
	prefix, epoch, seq, ok := scaling.ParseToken(inst.Tags[domain.TagLaunchToken])
	if !ok || pool == "" || prefix != scaling.TokenPrefix(l.Cluster, pool) {
		return fmt.Errorf("%w: launch token is not one of pool %q", ErrUnknownLaunch, pool)
	}
	led, err := l.Ledger(ctx, pool)
	if err != nil {
		return fmt.Errorf("reading the launch ledger of pool %q: %w", pool, err)
	}
	if led.Epoch == "" || epoch != led.Epoch || seq >= led.Next {
		return fmt.Errorf("%w: launch token is not in the ledger of pool %q", ErrUnknownLaunch, pool)
	}
	return nil
}
