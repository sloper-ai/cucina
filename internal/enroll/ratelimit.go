// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"context"
	"net"
	"time"

	"github.com/maypok86/otter/v2"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/peer"

	"github.com/sloper-ai/cucina/internal/ports"
)

// sourceLimiter is a token bucket per caller address (R-SEC-3 "rate-limited per
// source"). Buckets idle for 10 minutes are forgotten; the table is bounded.
type sourceLimiter struct {
	buckets *otter.Cache[string, *rate.Limiter]
	limit   rate.Limit
	burst   int
	clock   ports.Clock
}

func newSourceLimiter(perMinute, burst int, clock ports.Clock) *sourceLimiter {
	return &sourceLimiter{
		buckets: otter.Must(&otter.Options[string, *rate.Limiter]{
			MaximumSize:      100_000,
			ExpiryCalculator: otter.ExpiryAccessing[string, *rate.Limiter](10 * time.Minute),
		}),
		limit: rate.Limit(float64(perMinute) / 60),
		burst: burst,
		clock: clock,
	}
}

// allow takes one token for the caller of ctx.
func (l *sourceLimiter) allow(ctx context.Context) bool {
	src := sourceOf(ctx)
	b, _ := l.buckets.SetIfAbsent(src, rate.NewLimiter(l.limit, l.burst))
	return b.AllowN(l.clock.Now(), 1)
}

// sourceOf is the caller's IP address (port stripped), "unknown" without peer info.
func sourceOf(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return p.Addr.String()
	}
	return host
}
