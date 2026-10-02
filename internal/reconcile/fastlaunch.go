// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

const (
	// fastLaunchDescribeEvery bounds DescribeFastLaunchImages calls per image.
	fastLaunchDescribeEvery = time.Minute
	// defaultFastLaunchParallel is the AWS minimum for MaxParallelLaunches.
	defaultFastLaunchParallel = 6
)

// FastLaunchManager keeps EC2 Fast Launch enabled on the current AMI of each
// Windows pool and disabled everywhere else (R-POOL-2, R-OPS-2): enable on a
// new image first (its pre-provisioned snapshots are an approved standing
// cost), disable on the previous image after a rollout, and disable on every
// image before the pool goes away — waiting until AWS reports "disabled", so
// no snapshots or prep resources linger. The images it enabled are recorded
// (write-ahead) in the AnnFastLaunchImages annotation, so a restarted
// controller still cleans them up.
type FastLaunchManager struct {
	Compute ports.Compute
	Client  client.Client
	Clock   ports.Clock
	Log     *slog.Logger
	// Record receives the pre-provisioned snapshot count of each image the
	// manager looked at (0 once disabled): standing cost (R-OBS-5).
	Record func(pool domain.PoolName, image string, snapshots, sizeGiB int)

	mu    sync.Mutex
	state map[string]flState // by image ID
}

type flState struct {
	status ports.FastLaunchStatus
	at     time.Time
}

// Sync converges Fast Launch for one Windows pool. done is true when no
// recorded image other than the wanted one remains (finalizers wait for it).
func (m *FastLaunchManager) Sync(ctx context.Context, wp *v1alpha1.WorkerPool, rt *PoolRuntime, snap Snapshot, deleting bool) (done bool, err error) {
	recorded := splitList(wp.Annotations[AnnFastLaunchImages])
	fl := wp.Spec.EC2.FastLaunch
	want := ""
	if !deleting && fl != nil && fl.Enabled && rt.ImageErr == nil && rt.EC2.ImageID != "" {
		want = rt.EC2.ImageID
	}
	if want != "" && !slices.Contains(recorded, want) {
		recorded = append(recorded, want)
		if err := patchAnnotation(ctx, m.Client, wp.Namespace, wp.Name, AnnFastLaunchImages, strings.Join(recorded, ",")); err != nil {
			return false, err
		}
	}
	var errs []error
	if want != "" {
		st, err := m.describe(ctx, want)
		if err == nil && m.Record != nil {
			m.Record(domain.PoolName(wp.Name), want, st.Snapshots, rt.ImageSizeGiB)
		}
		switch {
		case err != nil:
			errs = append(errs, err)
		case st.State == "" || st.State == "disabled" || st.State == "failed":
			target := int(wp.Spec.Capacity.Max)
			if fl.TargetCount != nil {
				target = int(*fl.TargetCount)
			}
			parallel := defaultFastLaunchParallel
			if fl.MaxParallelLaunches != nil {
				parallel = max(int(*fl.MaxParallelLaunches), defaultFastLaunchParallel)
			}
			st, err := m.Compute.FastLaunch(ctx, ports.FastLaunchOp{Action: "enable", ImageID: want, TargetCount: max(target, 1), MaxParallel: parallel, LaunchTemplateID: fl.LaunchTemplateID})
			if err != nil {
				errs = append(errs, err)
			} else {
				m.remember(want, st)
				m.Log.Info("enabled EC2 Fast Launch", "pool", wp.Name, "image", want, "targetCount", target)
			}
		}
	}

	keep := []string{}
	done = true
	for _, img := range recorded {
		if img == want {
			keep = append(keep, img)
			continue
		}
		st, err := m.describe(ctx, img)
		switch {
		case errors.Is(err, ports.ErrImageNotFound) || errors.Is(err, ports.ErrNotFound):
			continue // image gone: nothing left to clean up
		case err != nil:
			errs = append(errs, err)
			keep, done = append(keep, img), false
		case st.State == "disabled" || st.State == "":
			m.Log.Info("EC2 Fast Launch disabled", "pool", wp.Name, "image", img)
			if m.Record != nil {
				m.Record(domain.PoolName(wp.Name), img, 0, 0)
			}
		case st.State == "disabling":
			keep, done = append(keep, img), false
		default:
			st, err := m.Compute.FastLaunch(ctx, ports.FastLaunchOp{Action: "disable", ImageID: img})
			if err != nil {
				errs = append(errs, err)
			} else {
				m.remember(img, st)
			}
			keep, done = append(keep, img), false
		}
	}
	if !slices.Equal(keep, recorded) {
		if err := patchAnnotation(ctx, m.Client, wp.Namespace, wp.Name, AnnFastLaunchImages, strings.Join(keep, ",")); err != nil {
			errs = append(errs, err)
		}
	}
	return done, errors.Join(errs...)
}

func (m *FastLaunchManager) describe(ctx context.Context, image string) (ports.FastLaunchStatus, error) {
	now := m.Clock.Now()
	m.mu.Lock()
	s, ok := m.state[image]
	m.mu.Unlock()
	if ok && now.Sub(s.at) < fastLaunchDescribeEvery {
		return s.status, nil
	}
	st, err := m.Compute.FastLaunch(ctx, ports.FastLaunchOp{Action: "describe", ImageID: image})
	if err != nil {
		return st, err
	}
	m.remember(image, st)
	return st, nil
}

func (m *FastLaunchManager) remember(image string, st ports.FastLaunchStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		m.state = map[string]flState{}
	}
	m.state[image] = flState{status: st, at: m.Clock.Now()}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
