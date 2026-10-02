// SPDX-License-Identifier: FSL-1.1-ALv2

package scalein_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/collect/scalein"
)

// Guards T8/NFR-C1: every observed worker needs continuous idle, acknowledged
// drain and provider-confirmed termination evidence within the actual policy.
// Missing records/API errors cannot become zero-latency successful scale-in.
func TestScaleInEvidence(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0).UTC()
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	duration := func(n int) *time.Duration { v := time.Duration(n) * time.Second; return &v }
	policies := map[string]scalein.Policy{"linux": {IdleTimeout: 30 * time.Second, DrainGrace: 20 * time.Second}, "windows": {IdleTimeout: 30 * time.Second, DrainGrace: 20 * time.Second}}
	base := func() []scalein.Sample {
		return []scalein.Sample{
			{Started: at(0), Finished: at(1), Workers: []scalein.Worker{{Node: "a", Pool: "linux", State: "busy", Busy: 1}}, Queues: map[string]scalein.Queue{"linux": {Queued: 1}}, Instances: []scalein.Instance{{Node: "a", Pool: "linux", State: "running"}}},
			{Started: at(10), Finished: at(11), Workers: []scalein.Worker{{Node: "a", Pool: "linux", State: "idle", IdleFor: duration(5)}}, Queues: map[string]scalein.Queue{"linux": {}}, Instances: []scalein.Instance{{Node: "a", Pool: "linux", State: "running"}}},
			{Started: at(35), Finished: at(36), Workers: []scalein.Worker{{Node: "a", Pool: "linux", State: "draining", Drained: true}}, Queues: map[string]scalein.Queue{"linux": {}}, Instances: []scalein.Instance{{Node: "a", Pool: "linux", State: "shutting-down"}}},
			{Started: at(40), Finished: at(41), Queues: map[string]scalein.Queue{"linux": {}}, Instances: []scalein.Instance{{Node: "a", Pool: "linux", State: "terminated"}}},
		}
	}
	for _, tc := range []struct {
		name            string
		change          func([]scalein.Sample) []scalein.Sample
		pass, violation bool
	}{
		{name: "complete", pass: true},
		{name: "empty cohort", change: func([]scalein.Sample) []scalein.Sample { return nil }},
		{name: "missing idle duration", change: func(s []scalein.Sample) []scalein.Sample { s[1].Workers[0].IdleFor = nil; return s }},
		{name: "missing queue is not empty", change: func(s []scalein.Sample) []scalein.Sample { s[1].Queues = nil; return s }},
		{name: "missing drain proof", change: func(s []scalein.Sample) []scalein.Sample { s[2].Workers[0].Drained = false; return s }},
		{name: "successful drain event fills a poll gap", pass: true, change: func(s []scalein.Sample) []scalein.Sample {
			s[2].Workers = nil
			s[3].Drains = []scalein.Drain{{Node: "a", Pool: "linux", At: at(34), Acknowledged: true}}
			return s
		}},
		{name: "legacy drain intent is not acknowledgement", change: func(s []scalein.Sample) []scalein.Sample {
			s[2].Workers = nil
			s[3].Drains = []scalein.Drain{{Node: "a", Pool: "linux", At: at(34)}}
			return s
		}},
		{name: "new terminal worker cannot disappear from cohort", change: func(s []scalein.Sample) []scalein.Sample {
			s[3].Instances = append(s[3].Instances, scalein.Instance{Node: "short-lived", Pool: "linux", State: "terminated", Launched: at(20)})
			return s
		}},
		{name: "pre-existing terminal worker is outside cohort", pass: true, change: func(s []scalein.Sample) []scalein.Sample {
			for i := range s {
				s[i].Instances = append(s[i].Instances, scalein.Instance{Node: "old", Pool: "linux", State: "terminated", Launched: at(-100)})
			}
			return s
		}},
		{name: "disappearance is not termination", change: func(s []scalein.Sample) []scalein.Sample { s[3].Instances = nil; return s }},
		{name: "API outage is not termination", change: func(s []scalein.Sample) []scalein.Sample { s[3].Errors = []string{"provider unavailable"}; return s }},
		{name: "busy invalidates stale idle episode", change: func(s []scalein.Sample) []scalein.Sample {
			s[2].Workers[0] = scalein.Worker{Node: "a", Pool: "linux", State: "busy", Busy: 1}
			return s
		}},
		{name: "still running after deadline", violation: true, change: func(s []scalein.Sample) []scalein.Sample {
			s[2].Started, s[2].Finished = at(80), at(81)
			s[3].Started, s[3].Finished = at(90), at(91)
			return s
		}},
		{name: "sampling straddles deadline", change: func(s []scalein.Sample) []scalein.Sample { s[3].Started, s[3].Finished = at(54), at(56); return s }},
		{name: "partial cohort cannot pass", change: func(s []scalein.Sample) []scalein.Sample {
			s[0].Instances = append(s[0].Instances, scalein.Instance{Node: "b", Pool: "windows", State: "running"})
			return s
		}},
		{name: "late missing source invalidates complete coverage", change: func(s []scalein.Sample) []scalein.Sample { s[3].Errors = []string{"daemon snapshot missing"}; return s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			if tc.change != nil {
				s = tc.change(s)
			}
			r := scalein.Evaluate(policies, s)
			require.Equal(t, tc.pass, r.Pass)
			if tc.pass {
				require.Empty(t, r.Unavailable)
				require.Empty(t, r.Violations)
				require.Len(t, r.Nodes, 1)
				require.Equal(t, at(5), r.Nodes[0].IdleEarliest)
				require.Equal(t, at(41), r.Nodes[0].TerminatedBy)
				require.Equal(t, 36*time.Second, r.Nodes[0].ElapsedUpper)
			} else if tc.violation {
				require.NotEmpty(t, r.Violations)
			} else {
				require.NotEmpty(t, r.Unavailable)
			}
		})
	}
}
