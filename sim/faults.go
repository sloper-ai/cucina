// SPDX-License-Identifier: FSL-1.1-ALv2

package sim

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
)

var (
	// errAmbiguous is an API failure the caller cannot classify (timeout,
	// connection reset): the request may or may not have been processed.
	errAmbiguous = errors.New("simulated: connection reset by peer")
	// errUnavailable is a network cut between the controller and the scheduler.
	errUnavailable = errors.New("simulated: scheduler unavailable")
)

var buildQueueOps = []string{"ListPlatformQueues", "ListWorkers", "AddDrain", "RemoveDrain", "ListDrains", "KillOperations",
	"ListOperations", "GetOperation"}

func (w *world) applyFaults(now time.Time) {
	for len(w.faults) > 0 && !w.start.Add(w.faults[0].At.D()).After(now) {
		f := w.faults[0]
		w.faults = w.faults[1:]
		w.applyFault(f, now)
	}
}

// azs returns the availability zones of the simulated region.
func (w *world) azs() []string {
	var out []string
	for _, az := range fakes.DefaultComputeConfig().Subnets {
		if !slices.Contains(out, az) {
			out = append(out, az)
		}
	}
	slices.Sort(out)
	return out
}

func (w *world) applyFault(f Fault, now time.Time) {
	w.logf("fault %s pool=%s type=%s host=%s for %s", f.Kind, f.Pool, f.Type, f.Host, f.Duration.D())
	dur := f.Duration.D()
	restore := func(fn func()) {
		if dur > 0 {
			w.after(dur, fn)
		}
	}
	switch f.Kind {
	case FaultICE:
		var types []string
		switch {
		case f.Type != "":
			types = []string{f.Type}
		default:
			for _, p := range w.pools {
				if f.Pool == "" || p.def.Name == f.Pool {
					types = append(types, p.def.InstanceTypes...)
				}
			}
		}
		azs := w.azs()
		if f.AZ != "" {
			azs = []string{f.AZ}
		}
		for _, t := range types {
			for _, az := range azs {
				w.compute.SetCapacity(t, az, 0)
			}
		}
		restore(func() {
			for _, t := range types {
				for _, az := range azs {
					w.compute.SetCapacity(t, az, -1)
				}
			}
		})
	case FaultQuota:
		w.compute.SetVCPUQuota(int(f.Value))
		restore(func() { w.compute.SetVCPUQuota(w.sc.Fleet.Compute.VCPUQuota) })
	case FaultThrottle:
		w.compute.SetBucket("Launch", f.Value, f.Rate)
		restore(func() {
			b := w.sc.Fleet.Compute
			if b.LaunchBurst > 0 {
				w.compute.SetBucket("Launch", b.LaunchBurst, max(b.LaunchRate, 0.1))
				return
			}
			w.compute.SetBucket("Launch", 5, 2)
		})
	case FaultAPIError:
		op := f.Op
		if op == "" {
			op = "Launch"
		}
		p := f.Value
		if p <= 0 {
			p = 1
		}
		w.compute.FailRate(op, p, errAmbiguous)
		restore(func() { w.compute.FailRate(op, 0, nil) })
	case FaultControllerRestart:
		if f.MidScale {
			w.pendingMS = true
		} else {
			w.restartController(false)
		}
	case FaultWorkerDeath:
		for _, node := range w.pick(w.poolNodes(f.Pool, false), max(f.Count, 1)) {
			w.bq.RemoveNode(node)
		}
	case FaultSpotInterruption:
		for _, id := range w.pick(w.poolNodes(f.Pool, true), max(f.Count, 1)) {
			_ = w.compute.Interrupt(id)
		}
	case FaultNetworkCut:
		for _, op := range buildQueueOps {
			w.bq.FailRate(op, 1, errUnavailable)
		}
		restore(func() {
			for _, op := range buildQueueOps {
				w.bq.FailRate(op, 0, nil)
			}
		})
	case FaultSchedulerRestart:
		w.bq.Restart()
	case FaultHostOffline:
		w.hosts.SetOnline(f.Host, false)
		restore(func() { w.hosts.SetOnline(f.Host, true) })
	case FaultHostReboot:
		w.hosts.Reboot(f.Host)
	case FaultImageMissing:
		until := now.Add(dur)
		if dur <= 0 {
			until = now.Add(1000 * time.Hour)
		}
		w.imageMissing[f.Pool] = until
		if p := w.byName[f.Pool]; p != nil && p.imageID != "" {
			w.compute.RemoveImage(p.imageID)
			restore(func() {
				w.compute.AddImage(ports.Image{ID: p.imageID, Name: p.def.Name, Platform: p.def.Image.Platform, Generation: p.spec.Generation})
			})
		}
	case FaultStartFailure:
		until := now.Add(dur)
		if dur <= 0 {
			until = now.Add(1000 * time.Hour)
		}
		w.startFailure[f.Pool] = until
	case FaultLeak:
		w.compute.SetLeakOnTerminate(max(f.Value, 0.5))
		restore(func() { w.compute.SetLeakOnTerminate(0) })
	case FaultRollout:
		p := w.byName[f.Pool]
		gen := f.Op
		if gen == "" {
			gen = p.spec.Generation + "-next"
		}
		p.spec.Generation = gen
		if f.Type != "" {
			p.spec.Rollout = domain.RolloutPolicy(f.Type)
		}
		if p.imageID != "" {
			p.imageID = "ami-" + p.def.Name + "-" + gen
			w.compute.AddImage(ports.Image{ID: p.imageID, Name: p.def.Name, Platform: p.def.Image.Platform, Generation: gen, Version: gen})
		}
	case FaultDelete:
		w.byName[f.Pool].spec.Deleting = true
	default:
		panic(fmt.Sprintf("unknown fault %q", f.Kind))
	}
}

// poolNodes lists the pool's registered nodes (workers) or, with instances,
// its live EC2 instances.
func (w *world) poolNodes(pool string, instances bool) []string {
	var out []string
	if instances {
		for _, in := range w.compute.All() {
			if (pool == "" || string(in.Pool) == pool) && (in.State == ports.InstancePending || in.State == ports.InstanceRunning) {
				out = append(out, in.ID)
			}
		}
		return out
	}
	poolOf := w.nodePools()
	for _, n := range w.bq.Nodes() {
		if p := poolOf[n]; p != nil && (pool == "" || p.def.Name == pool) {
			out = append(out, n)
		}
	}
	return out
}

// pick chooses n distinct items deterministically from the world's PRNG.
func (w *world) pick(items []string, n int) []string {
	items = slices.Clone(items)
	slices.Sort(items)
	r := w.rnd.Child(fmt.Sprintf("pick-%d", w.clock.Now().UnixNano()))
	for i := len(items) - 1; i > 0; i-- {
		j := int(r.Int63n(int64(i + 1)))
		items[i], items[j] = items[j], items[i]
	}
	return items[:min(n, len(items))]
}
