// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
	"pgregory.net/rapid"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/workeragent"
)

// Guards: R-POOL-7 / NFR-R4 — the dead-man switch powers a worker off when it
// is idle beyond 30 min, cannot reach the scheduler for 10 min, or is up for
// 12 h, from local observations only (it works with the controller gone).
func TestDecideDeadman(t *testing.T) {
	l := workeragent.LimitsFromSettings(nil) // defaults
	require.Equal(t, workeragent.DeadmanLimits{Idle: 30 * time.Minute, Unreachable: 10 * time.Minute, MaxUptime: 12 * time.Hour}, l)
	m := time.Minute
	cases := []struct {
		name string
		obs  workeragent.DeadmanObservation
		want string
	}{
		{"fresh worker", workeragent.DeadmanObservation{Uptime: m}, ""},
		{"busy and connected", workeragent.DeadmanObservation{Uptime: 11 * time.Hour, SinceActivity: 0, SinceContact: 30 * time.Second}, ""},
		{"idle just below the limit", workeragent.DeadmanObservation{Uptime: time.Hour, SinceActivity: 30*m - time.Second}, ""},
		{"idle at the limit", workeragent.DeadmanObservation{Uptime: time.Hour, SinceActivity: 30 * m}, workeragent.ReasonIdle},
		{"controller and scheduler gone", workeragent.DeadmanObservation{Uptime: time.Hour, SinceActivity: 12 * m, SinceContact: 10 * m}, workeragent.ReasonUnreachable},
		{"scheduler blip shorter than the limit", workeragent.DeadmanObservation{Uptime: time.Hour, SinceContact: 9 * m}, ""},
		{"busy beyond maximum uptime", workeragent.DeadmanObservation{Uptime: 12 * time.Hour}, workeragent.ReasonMaxUptime},
		{"every limit exceeded", workeragent.DeadmanObservation{Uptime: 13 * time.Hour, SinceActivity: time.Hour, SinceContact: time.Hour}, workeragent.ReasonMaxUptime},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := workeragent.DecideDeadman(l, tc.obs)
			require.Equal(t, tc.want != "", v.PowerOff)
			require.Equal(t, tc.want, v.Reason)
		})
	}

	custom := workeragent.LimitsFromSettings(&cucinav1.DeadmanSettings{IdleLimit: durationpb.New(5 * m), MaxUptime: durationpb.New(0)})
	require.Equal(t, workeragent.DeadmanLimits{Idle: 5 * m, Unreachable: 10 * m, MaxUptime: 12 * time.Hour}, custom, "unset fields take defaults")
}

// Guards: R-POOL-7 — the verdict is "power off" exactly when some limit is
// reached, and more idleness/unreachability/uptime never revives a worker.
func TestDecideDeadmanProperties(t *testing.T) {
	dur := rapid.Int64Range(0, int64(48*time.Hour))
	rapid.Check(t, func(rt *rapid.T) {
		l := workeragent.DeadmanLimits{
			Idle:        time.Duration(rapid.Int64Range(1, int64(2*time.Hour)).Draw(rt, "idle")),
			Unreachable: time.Duration(rapid.Int64Range(1, int64(2*time.Hour)).Draw(rt, "unreachable")),
			MaxUptime:   time.Duration(rapid.Int64Range(1, int64(24*time.Hour)).Draw(rt, "maxUptime")),
		}
		o := workeragent.DeadmanObservation{
			Uptime: time.Duration(dur.Draw(rt, "uptime")), SinceActivity: time.Duration(dur.Draw(rt, "activity")),
			SinceContact: time.Duration(dur.Draw(rt, "contact")),
		}
		v := workeragent.DecideDeadman(l, o)
		exceeded := o.Uptime >= l.MaxUptime || o.SinceActivity >= l.Idle || o.SinceContact >= l.Unreachable
		require.Equal(rt, exceeded, v.PowerOff)
		more := o
		more.Uptime += time.Duration(dur.Draw(rt, "moreUptime"))
		more.SinceActivity += time.Duration(dur.Draw(rt, "moreIdle"))
		more.SinceContact += time.Duration(dur.Draw(rt, "moreUnreachable"))
		if v.PowerOff {
			require.True(rt, workeragent.DecideDeadman(l, more).PowerOff)
		}
	})
}
