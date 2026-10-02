// SPDX-License-Identifier: FSL-1.1-ALv2

package invariants

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// MaxVMsPerHost is Apple's limit of concurrently running macOS VMs per host.
const MaxVMsPerHost = 2

// RequiredTags are the tag keys every controller-created resource carries
// (docs/contracts.md §5.2).
var RequiredTags = []string{
	domain.TagManagedBy, domain.TagCluster, domain.TagPool,
	domain.TagGeneration, domain.TagLaunchToken, domain.TagRole,
}

// ------------------------------------------------- action guards (pre-execution)

// LaunchWithinMax guards a launch: the pool's active VMs (launching +
// registered and not retiring) plus launches in flight plus the new launches
// must not exceed max.
func LaunchWithinMax(pool string, maxVMs, active, inflight, launches int) []Violation {
	if launches <= 0 || active+inflight+launches <= maxVMs {
		return nil
	}
	return []Violation{violation(InstancesNeverExceedMax, pool, "",
		"launching %d with %d active and %d in flight exceeds max %d", launches, active, inflight, maxVMs)}
}

// StopAllowed guards a terminate/stop of one VM: none of its threads may be
// executing unless the VM has been draining for at least drainTimeout
// (drainStart zero means it is not draining).
func StopAllowed(pool string, vm domain.VM, now, drainStart time.Time, drainTimeout time.Duration) []Violation {
	if vm.Busy == 0 {
		return nil
	}
	if !drainStart.IsZero() && drainTimeout > 0 && now.Sub(drainStart) >= drainTimeout {
		return nil
	}
	return []Violation{violation(NeverTerminateBusyOrLeased, pool, vm.ID,
		"%d of %d threads executing and drain timeout not expired", vm.Busy, vm.Threads)}
}

// IdleScaleInAllowed guards an idle scale-in: the VM must have been idle and
// the pool's queues empty for at least idleTimeout.
func IdleScaleInAllowed(pool string, vm domain.VM, now, queuesEmptySince time.Time, idleTimeout time.Duration) []Violation {
	switch {
	case vm.Busy > 0:
		return []Violation{violation(ScaleInOnlyAfterIdleTimeout, pool, vm.ID, "VM is busy")}
	case vm.IdleSince.IsZero() || now.Sub(vm.IdleSince) < idleTimeout:
		return []Violation{violation(ScaleInOnlyAfterIdleTimeout, pool, vm.ID, "idle for %s < %s", since(now, vm.IdleSince), idleTimeout)}
	case queuesEmptySince.IsZero() || now.Sub(queuesEmptySince) < idleTimeout:
		return []Violation{violation(ScaleInOnlyAfterIdleTimeout, pool, vm.ID, "queues empty for %s < %s", since(now, queuesEmptySince), idleTimeout)}
	}
	return nil
}

func since(now, t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return now.Sub(t)
}

// ----------------------------------------------- snapshot checks (ground truth)

// CheckInstances checks a fleet listing: every instance of the cluster is
// fully tagged, none is stopped, and no two share an idempotency token.
// Terminated instances are included in the duplicate check (a duplicate that
// was already terminated is still a double launch).
func CheckInstances(cluster string, instances []ports.Instance) []Violation {
	var out []Violation
	byToken := map[string][]string{}
	for _, in := range instances {
		if in.Tags[domain.TagCluster] != cluster {
			continue
		}
		pool := string(in.Pool)
		var missing []string
		for _, k := range RequiredTags {
			if in.Tags[k] == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			out = append(out, violation(EveryResourceTagged, pool, in.ID, "missing tags %v", missing))
		}
		if in.State == ports.InstanceStopped || in.State == ports.InstanceStopping {
			out = append(out, violation(NoStoppedEC2Instances, pool, in.ID, "instance is %s", in.State))
		}
		if tok := in.Tags[domain.TagLaunchToken]; tok != "" {
			byToken[tok] = append(byToken[tok], in.ID)
		}
	}
	toks := make([]string, 0, len(byToken))
	for t := range byToken {
		toks = append(toks, t)
	}
	sort.Strings(toks)
	for _, t := range toks {
		if ids := byToken[t]; len(ids) > 1 {
			slices.Sort(ids)
			out = append(out, violation(NoDuplicateLaunchPerToken, "", t, "instances %v share one launch token", ids))
		}
	}
	return out
}

// CheckResourceTags checks the tags of a volume or ENI the controller created
// (instances are covered by CheckInstances).
func CheckResourceTags(kind, id string, tags map[string]string) []Violation {
	var missing []string
	for _, k := range RequiredTags {
		if tags[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Violation{violation(EveryResourceTagged, tags[domain.TagPool], kind+"/"+id, "missing tags %v", missing)}
}

// CheckPoolMax checks that a pool's live, non-retiring VMs do not exceed max.
func CheckPoolMax(pool string, maxVMs, liveNonRetiring int) []Violation {
	if liveNonRetiring <= maxVMs {
		return nil
	}
	return []Violation{violation(InstancesNeverExceedMax, pool, "", "%d live VMs exceed max %d", liveNonRetiring, maxVMs)}
}

// CheckHostVMs checks the 2-VM limit on every host (running VMs only, R-MAC-3).
func CheckHostVMs(hosts []ports.HostState) []Violation {
	var out []Violation
	for _, h := range hosts {
		n := h.RunningVMs
		if len(h.VMs) > 0 {
			n = 0
			for _, vm := range h.VMs {
				if vm.State != domain.VMStopped && vm.State != domain.VMTerminated && vm.State != domain.VMFailed {
					n++
				}
			}
		}
		if n > MaxVMsPerHost {
			out = append(out, violation(AtMostTwoVMsPerHost, "", h.Serial, "%d VMs running", n))
		}
	}
	return out
}

// CheckOrphans reports pool volumes/ENIs that stayed orphaned beyond grace.
func CheckOrphans(orphans []ports.Orphan, grace time.Duration) []Violation {
	var out []Violation
	for _, o := range orphans {
		if o.Age > grace {
			out = append(out, violation(NoOrphanVolumesBeyondGrace, string(o.Pool), fmt.Sprintf("%s/%s", o.Kind, o.ID),
				"orphaned for %s > grace %s", o.Age, grace))
		}
	}
	return out
}
