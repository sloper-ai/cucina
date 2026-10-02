// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"time"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
)

// Dead-man switch defaults (R-POOL-7). The controller's idle policy is
// strictly tighter (idleTimeout 5–10 min), so the switch only fires when the
// controller is gone or wrong.
const (
	DefaultIdleLimit        = 30 * time.Minute
	DefaultUnreachableLimit = 10 * time.Minute
	DefaultMaxUptime        = 12 * time.Hour
)

// DeadmanLimits are the dead-man thresholds.
type DeadmanLimits struct {
	Idle        time.Duration
	Unreachable time.Duration
	MaxUptime   time.Duration
}

// LimitsFromSettings applies the defaults to unset (nil, zero or negative) fields.
func LimitsFromSettings(d *cucinav1.DeadmanSettings) DeadmanLimits {
	pick := func(v time.Duration, def time.Duration) time.Duration {
		if v <= 0 {
			return def
		}
		return v
	}
	return DeadmanLimits{
		Idle:        pick(d.GetIdleLimit().AsDuration(), DefaultIdleLimit),
		Unreachable: pick(d.GetUnreachableLimit().AsDuration(), DefaultUnreachableLimit),
		MaxUptime:   pick(d.GetMaxUptime().AsDuration(), DefaultMaxUptime),
	}
}

// DeadmanObservation is what the worker knows about itself, measured locally
// (never from the controller): time since boot, since the last build
// activity of bb_worker and since the last successful scheduler contact.
type DeadmanObservation struct {
	Uptime        time.Duration
	SinceActivity time.Duration
	SinceContact  time.Duration
}

// Power-off reasons.
const (
	ReasonMaxUptime   = "max-uptime"
	ReasonUnreachable = "unreachable"
	ReasonIdle        = "idle"
)

// DeadmanVerdict is the decision; Reason is "" while the worker may live.
type DeadmanVerdict struct {
	PowerOff bool
	Reason   string
}

// DecideDeadman is the dead-man switch (R-POOL-7, NFR-R4): power off once the
// worker has been idle for Idle, unable to reach the scheduler for
// Unreachable, or up for MaxUptime — whichever comes first. It depends on
// local observations only, so it keeps working when the controller is gone.
// When several limits are exceeded the reason is the first of max-uptime,
// unreachable, idle.
func DecideDeadman(l DeadmanLimits, o DeadmanObservation) DeadmanVerdict {
	switch {
	case o.Uptime >= l.MaxUptime:
		return DeadmanVerdict{PowerOff: true, Reason: ReasonMaxUptime}
	case o.SinceContact >= l.Unreachable:
		return DeadmanVerdict{PowerOff: true, Reason: ReasonUnreachable}
	case o.SinceActivity >= l.Idle:
		return DeadmanVerdict{PowerOff: true, Reason: ReasonIdle}
	}
	return DeadmanVerdict{}
}
