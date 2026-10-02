// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"errors"

	"github.com/sloper-ai/cucina/internal/buildqueue"
	"github.com/sloper-ai/cucina/internal/ports"
)

// BuildQueueState client (agent `bbconfig`): the autoscaler's signal and the
// drain/kill control, reachable only over mTLS with the controller's client
// certificate (R-SEC-4). The client TLS configuration reloads the certificate
// and verifies the scheduler against the current CA bundle.
func init() {
	ProvideBuildQueue(func(ctx context.Context, d *Deps) (ports.BuildQueue, error) {
		if d.TLS == nil || d.TLS.SchedulerClient == nil {
			return nil, errors.New("BuildQueueState needs /scheduler/clientCertFile, clientKeyFile and /tls/caFile (mTLS, R-SEC-4)")
		}
		return buildqueue.New(buildqueue.Options{Address: d.Config.Scheduler.BuildQueueStateAddress, TLS: d.TLS.SchedulerClient})
	})
}
