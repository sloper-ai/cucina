// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/ports"
)

// R-SCALE-6: RunInstances is metered by a client-side bucket (burst 5, refill 2/s):
// nine launches need four refills, i.e. 2 s; throttling drains the bucket and a
// Retry-After hint blocks it for that long.
func TestRunInstancesTokenBucket(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for i := range 9 {
		_, err := e.p.Launch(ctx, launchReq(fmt.Sprintf("tok-%d", i)))
		require.NoError(t, err)
	}
	assert.Equal(t, 2*time.Second, e.clock.Slept())

	e.clock.Advance(time.Minute) // bucket full again
	before := e.clock.Slept()
	throttled := &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"3"}}}},
		Err:      &smithy.GenericAPIError{Code: "RequestLimitExceeded"},
	}
	e.ec2.failOnce("RunInstances", throttled)
	_, err := e.p.Launch(ctx, launchReq("tok-throttled"))
	var te *ThrottleError
	require.ErrorAs(t, err, &te)
	assert.ErrorIs(t, err, ports.ErrThrottled)
	assert.Equal(t, 3*time.Second, te.RetryAfter)

	_, err = e.p.Launch(ctx, launchReq("tok-after"))
	require.NoError(t, err)
	assert.Equal(t, 3*time.Second, e.clock.Slept()-before, "the next launch waits for Retry-After")
}

// R-SCALE-4/6: the SDK retryer never retries capacity errors (EC2 returns ICE with
// a 5xx status), so ICE comes back at once; throttling and server errors are retried.
func TestRetryerDoesNotRetryCapacity(t *testing.T) {
	r := newRetryer()
	for code, want := range map[string]bool{
		"InsufficientInstanceCapacity": false,
		"Unsupported":                  false,
		"VcpuLimitExceeded":            false,
		"IdempotentParameterMismatch":  false,
		"RequestLimitExceeded":         true,
		"InternalError":                true,
	} {
		err := &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 500}},
			Err:      &smithy.GenericAPIError{Code: code},
		}
		assert.Equal(t, want, r.IsErrorRetryable(err), code)
	}
	assert.Greater(t, r.MaxAttempts(), 1)
	_ = aws.Retryer(r)
}
