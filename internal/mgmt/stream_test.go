// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/mgmt"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Streaming tests run in a synctest bubble: time is virtual, so cadence, rate
// limits and follow limits are checked deterministically without sleeping.

// advance moves the bubble's virtual clock forward by d: this goroutine waits on a
// timer, and synctest jumps the fake clock once every goroutine in the bubble is
// blocked. It must only be called inside synctest.Test.
func advance(d time.Duration) { <-time.After(d) }

// TestWatchOverviewSharesSnapshots guards R-CLI-4 (≤ 2 s refresh) and the cost of
// the TUI: 20 subscribers over 10 s each get an overview every 2 s, while the pools
// view and the scheduler are read once per tick, not once per subscriber.
func TestWatchOverviewSharesSnapshots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(as(reader))
		const subscribers = 20
		streams := make([]*fakeStream[cucinav1.Overview], subscribers)
		var wg sync.WaitGroup
		for i := range streams {
			streams[i] = newStream[cucinav1.Overview](ctx)
			wg.Go(func() { _ = f.srv.WatchOverview(&cucinav1.WatchOverviewRequest{}, streams[i]) })
		}
		advance(10 * time.Second)
		synctest.Wait()
		cancel()
		wg.Wait()

		for i, s := range streams {
			got := s.drain()
			if len(got) != 6 { // t = 0, 2, 4, 6, 8, 10 s
				t.Errorf("subscriber %d got %d overviews in 10 s, want 6", i, len(got))
			}
			if len(got) > 0 && (len(got[0].GetPools()) != 3 || got[0].GetWorkersByState()["busy"] != 1) {
				t.Errorf("overview content = %v", got[0])
			}
		}
		if n := f.fleet.poolCalls.Load(); n > 6 {
			t.Errorf("pools view read %d times for 20 subscribers over 6 ticks, want ≤ 6", n)
		}
		if n := f.sched.queueCalls.Load(); n > 6 {
			t.Errorf("scheduler queried %d times for 20 subscribers over 6 ticks, want ≤ 6", n)
		}
		if o, _, _, _ := f.srv.StreamCounts(); o != 0 {
			t.Errorf("watchers after cancel = %d", o)
		}
	})
}

// TestWatchOverviewBackpressureAndCancellation guards the stream bounds: a client
// that stops reading does not delay others, subscribers beyond the limit are
// refused, cancellation frees the slot, and shutdown ends streams with UNAVAILABLE.
func TestWatchOverviewBackpressureAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, func(_ *mgmt.Deps, o *mgmt.Options) { o.MaxOverviewWatchers = 2 })
		slowCtx, cancelSlow := context.WithCancel(as(reader))
		slow := newStream[cucinav1.Overview](slowCtx)
		slow.gate = make(chan struct{}) // never opened: the client never reads
		fast := newStream[cucinav1.Overview](as(reader))
		slowDone, fastDone := make(chan error, 1), make(chan error, 1)
		go func() { slowDone <- f.srv.WatchOverview(&cucinav1.WatchOverviewRequest{}, slow) }()
		go func() { fastDone <- f.srv.WatchOverview(&cucinav1.WatchOverviewRequest{}, fast) }()
		synctest.Wait()

		third := newStream[cucinav1.Overview](as(reader))
		if err := f.srv.WatchOverview(&cucinav1.WatchOverviewRequest{}, third); status.Code(err) != codes.ResourceExhausted {
			t.Errorf("third watcher: %v, want ResourceExhausted", err)
		}

		advance(6 * time.Second)
		synctest.Wait()
		if n := len(fast.drain()); n != 4 { // t = 0, 2, 4, 6 s despite the blocked client
			t.Errorf("fast client got %d overviews, want 4", n)
		}

		cancelSlow()
		synctest.Wait()
		select {
		case <-slowDone:
		default:
			t.Fatal("blocked watcher did not return after cancellation")
		}
		if o, _, _, _ := f.srv.StreamCounts(); o != 1 {
			t.Errorf("watchers after cancelling one = %d, want 1", o)
		}

		f.srv.Stop()
		if err := <-fastDone; status.Code(err) != codes.Unavailable {
			t.Errorf("watcher at shutdown: %v, want Unavailable", err)
		}
	})
}

// TestWatchOperationsStreamsDiffs guards UC13 (live operations): the stream starts
// with the visible operations as ADDED, then sends ADDED/CHANGED/REMOVED diffs,
// never shows operations of instance names the caller cannot see, and concurrent
// watchers share one scheduler listing per tick.
func TestWatchOperationsStreamsDiffs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		base := f.sched.ops
		ctx, cancel := context.WithCancel(as(reader))
		st := newStream[cucinav1.OperationEvent](ctx)
		other := newStream[cucinav1.OperationEvent](ctx)
		done := make(chan error, 2)
		go func() { done <- f.srv.WatchOperations(&cucinav1.ListOperationsRequest{InstanceName: "main"}, st) }()
		go func() { done <- f.srv.WatchOperations(&cucinav1.ListOperationsRequest{Stage: "queued"}, other) }()
		synctest.Wait()
		events := func(s *fakeStream[cucinav1.OperationEvent]) []string {
			var out []string
			for _, e := range s.drain() {
				out = append(out, strings.TrimPrefix(e.GetKind().String(), "KIND_")+" "+e.GetOperation().GetName()+" "+e.GetOperation().GetStage())
			}
			return out
		}
		equal(t, "initial", events(st), []string{"ADDED op-1 executing", "ADDED op-2 queued", "ADDED op-3 queued"})
		equal(t, "initial (queued, reader)", events(other), []string{"ADDED op-2 queued", "ADDED op-3 queued"})

		op2 := base[1]
		op2.Stage = "executing"
		f.sched.setOps(base[0], op2, base[3],
			ports.Operation{Name: "op-5", Queue: base[0].Queue, Stage: "queued"},
			ports.Operation{Name: "op-6", Queue: base[3].Queue, Stage: "queued"}) // tenant-b: invisible
		advance(2 * time.Second)
		synctest.Wait()
		equal(t, "diff", events(st), []string{"CHANGED op-2 executing", "REMOVED op-3 queued", "ADDED op-5 queued"})
		equal(t, "diff (queued)", events(other), []string{"REMOVED op-2 queued", "REMOVED op-3 queued", "ADDED op-5 queued"})
		if n := f.sched.opsCalls.Load(); n != 2 {
			t.Errorf("scheduler listed operations %d times for 2 watchers over 2 ticks, want 2", n)
		}
		cancel()
		for range 2 {
			if err := <-done; status.Code(err) != codes.Canceled {
				t.Errorf("watch after cancel: %v, want Canceled", err)
			}
		}
	})
}

// TestStreamWorkerLogs guards R-OBS-4 / R-CLI-3 (`workers logs`): EC2 logs come from
// SSM with follow-by-polling from the last cursor/offset, every poll is rate-limited
// and bounded, follow ends at the follow limit, Tart logs come from the host, output
// is redacted, and concurrent log streams are bounded.
func TestStreamWorkerLogs(t *testing.T) {
	const jwt = "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJlLXZhbHVl"
	tweak := func(_ *mgmt.Deps, o *mgmt.Options) {
		o.LogPollInterval = 5 * time.Second
		o.LogFollowLimit = 12 * time.Second
		o.SSMCallsPerSecond = 0.5 // one SendCommand every 2 s on average
		o.SSMBurst = 1
		o.MaxLogStreams = 1
	}
	collect := func(s *fakeStream[cucinav1.LogChunk]) (string, bool) {
		var b strings.Builder
		last := false
		for _, c := range s.drain() {
			b.Write(c.GetData())
			last = c.GetLast()
		}
		return b.String(), last
	}

	t.Run("linux follow", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newFixture(t, tweak)
			f.shell.reply = func(n int, _ string) string {
				switch n {
				case 0:
					return "09:00 start\n09:01 got Bearer " + jwt + "\n-- cursor: s=ab;i=2\n"
				case 1:
					return "09:02 action done\n-- cursor: s=ab;i=3\n"
				}
				return "" // nothing new: no cursor either
			}
			st := newStream[cucinav1.LogChunk](as(admin))
			err := f.srv.StreamWorkerLogs(&cucinav1.StreamWorkerLogsRequest{Node: "i-0aaaaaaaaaaaaaaa1", TailLines: 50, Follow: true}, st)
			if err != nil {
				t.Fatal(err)
			}
			text, last := collect(st)
			if !last || strings.Contains(text, jwt) || !strings.Contains(text, "09:02 action done") ||
				!strings.Contains(text, "[follow limit reached") || strings.Contains(text, "-- cursor") {
				t.Errorf("streamed log = %q (last %v)", text, last)
			}
			scripts := f.shell.calls()
			if len(scripts) != 4 { // t = 0, 5, 10, 15 s; the follow limit is 12 s
				t.Fatalf("SSM calls = %d, want 4", len(scripts))
			}
			if !strings.Contains(scripts[0], "-u bb-worker.service -n 50") ||
				!strings.Contains(scripts[1], "--after-cursor='s=ab;i=2'") || !strings.Contains(scripts[2], "--after-cursor='s=ab;i=3'") ||
				!strings.Contains(scripts[3], "--after-cursor='s=ab;i=3'") {
				t.Errorf("scripts = %q", scripts)
			}
		})
	})

	t.Run("windows offsets", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newFixture(t, tweak)
			f.shell.reply = func(n int, _ string) string {
				return []string{
					"w1\nw2\n-- offset: 100\n",
					"w3\n-- more\n-- offset: 22100\n",
					"w4\n-- offset: 22103\n",
				}[n]
			}
			st := newStream[cucinav1.LogChunk](as(admin))
			done := make(chan error, 1)
			go func() {
				done <- f.srv.StreamWorkerLogs(&cucinav1.StreamWorkerLogsRequest{Node: "i-0bbbbbbbbbbbbbbb1", Unit: "bb-runner", Follow: true}, st)
			}()
			advance(7 * time.Second) // t=0 initial, t=5 "more", then the next poll waits only for the rate limit
			synctest.Wait()
			text, _ := collect(st)
			equal(t, "log", text, "w1\nw2\nw3\nw4\n")
			scripts := f.shell.calls()
			if len(scripts) != 3 || !strings.Contains(scripts[0], `logs\bb-runner.log`) || !strings.Contains(scripts[0], "[int64]-1") ||
				!strings.Contains(scripts[1], "[int64]100") || !strings.Contains(scripts[2], "[int64]22100") {
				t.Errorf("scripts = %d %q", len(scripts), scripts)
			}
			if gap := f.shell.at[2].Sub(f.shell.at[1]); gap != 2*time.Second {
				t.Errorf("poll after a truncated read came after %v, want the 2 s rate limit", gap)
			}

			// Only one log stream at a time (MaxLogStreams = 1).
			other := newStream[cucinav1.LogChunk](as(admin))
			if err := f.srv.StreamWorkerLogs(&cucinav1.StreamWorkerLogsRequest{Node: "i-0aaaaaaaaaaaaaaa1"}, other); status.Code(err) != codes.ResourceExhausted {
				t.Errorf("second log stream: %v, want ResourceExhausted", err)
			}
			f.srv.Stop()
			if err := <-done; status.Code(err) != codes.Unavailable {
				t.Errorf("log stream at shutdown: %v, want Unavailable", err)
			}
		})
	})

	t.Run("tart snapshot", func(t *testing.T) {
		f := newFixture(t, tweak)
		f.hosts.diag["C02XYZ123456"] = "vm-1 bb_runner: token cuc_sk_k2_c2VjcmV0LWtleS0y used\n"
		st := newStream[cucinav1.LogChunk](as(admin))
		if err := f.srv.StreamWorkerLogs(&cucinav1.StreamWorkerLogsRequest{Node: "mini1/vm-1", Unit: "bb-runner", TailLines: 100}, st); err != nil {
			t.Fatal(err)
		}
		text, last := collect(st)
		if !last || strings.Contains(text, "cuc_sk_k2") || !strings.Contains(text, "vm-1 bb_runner") {
			t.Errorf("streamed log = %q (last %v)", text, last)
		}
		equal(t, "request", f.hosts.diagReqs[0], mgmt.DiagnosticsRequest{IncludeVMLogs: true, VM: "vm-1", Unit: "bb-runner", TailLines: 100})
		if len(f.shell.calls()) != 0 {
			t.Error("Tart worker logs went through SSM")
		}
	})

	t.Run("invalid unit", func(t *testing.T) {
		f := newFixture(t, tweak)
		st := newStream[cucinav1.LogChunk](as(admin))
		if err := f.srv.StreamWorkerLogs(&cucinav1.StreamWorkerLogsRequest{Node: "i-0aaaaaaaaaaaaaaa1", Unit: "sshd; cat /etc/shadow"}, st); status.Code(err) != codes.InvalidArgument {
			t.Errorf("unknown unit: %v, want InvalidArgument", err)
		}
	})
}
