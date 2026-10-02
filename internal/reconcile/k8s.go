// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// Annotations the controller keeps on WorkerPool objects. Helm leaves
// annotations it does not render alone, so they survive upgrades.
const (
	// AnnLedger holds the launch ledger (JSON scaling.Ledger), written ahead of
	// every launch so that a new leader reuses in-flight tokens (R-SCALE-5).
	AnnLedger = "cucina.sloper.ai/launch-ledger"
	// AnnFastLaunchImages lists the AMIs on which the controller enabled EC2 Fast
	// Launch (comma-separated); they are disabled before the pool goes away and
	// after a rollout (R-POOL-2, R-OPS-2).
	AnnFastLaunchImages = "cucina.sloper.ai/fast-launch-images"
)

// ReadLedger parses the ledger annotation; a missing or corrupt annotation
// yields the zero ledger (a fresh epoch; tokens can't collide with old ones).
func ReadLedger(wp *v1alpha1.WorkerPool) scaling.Ledger {
	var l scaling.Ledger
	if s := wp.Annotations[AnnLedger]; s != "" {
		if err := json.Unmarshal([]byte(s), &l); err != nil {
			return scaling.Ledger{}
		}
	}
	return l
}

// AnnotationLedgers persists ledgers as a WorkerPool annotation (merge patch).
type AnnotationLedgers struct {
	Client    client.Client
	Namespace string
}

// SaveLedger implements LedgerStore.
func (s AnnotationLedgers) SaveLedger(ctx context.Context, pool domain.PoolName, l scaling.Ledger) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return patchAnnotation(ctx, s.Client, s.Namespace, string(pool), AnnLedger, string(b))
}

func patchAnnotation(ctx context.Context, c client.Client, ns, name, key, value string) error {
	var v any = value
	if value == "" {
		v = nil // remove
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: v}}})
	if err != nil {
		return err
	}
	wp := &v1alpha1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := c.Patch(ctx, wp, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("patching annotation %s on WorkerPool %s: %w", key, name, err)
	}
	return nil
}

// RecorderEvents adapts an events.EventRecorder to EventSink; it remembers the
// pools' UIDs so events attach to the right object.
type RecorderEvents struct {
	Recorder  events.EventRecorder
	Namespace string

	mu   sync.Mutex
	uids map[domain.PoolName]types.UID
}

// Track records the UID of a pool (called by the reconciler).
func (r *RecorderEvents) Track(wp *v1alpha1.WorkerPool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.uids == nil {
		r.uids = map[domain.PoolName]types.UID{}
	}
	r.uids[domain.PoolName(wp.Name)] = wp.UID
}

// PoolEvent implements EventSink.
func (r *RecorderEvents) PoolEvent(pool domain.PoolName, warning bool, reason, message string) {
	if r == nil || r.Recorder == nil {
		return
	}
	r.mu.Lock()
	uid := r.uids[pool]
	r.mu.Unlock()
	obj := &v1alpha1.WorkerPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "WorkerPool"},
		ObjectMeta: metav1.ObjectMeta{Name: string(pool), Namespace: r.Namespace, UID: uid},
	}
	typ := corev1.EventTypeNormal
	if warning {
		typ = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(obj, nil, typ, reason, "Autoscale", "%s", message)
}

// statusPatch builds a JSON merge patch for the status subresource: the diff
// between before and after (so cleared fields become null) plus the given
// status fields, always. The CRDs mark non-omitempty status fields (desired,
// runningVMs, …) as required, so the first status write must carry them even
// when they are still zero; a plain diff would omit them and be rejected.
func statusPatch(before, after client.Object, required ...string) (client.Patch, error) {
	diff, err := client.MergeFrom(before).Data(after)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(diff, &m); err != nil {
		return nil, err
	}
	full, err := json.Marshal(after)
	if err != nil {
		return nil, err
	}
	var obj struct {
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(full, &obj); err != nil {
		return nil, err
	}
	st, _ := m["status"].(map[string]any)
	if st == nil {
		st = map[string]any{}
	}
	for _, k := range required {
		if v, ok := obj.Status[k]; ok {
			if _, set := st[k]; !set {
				st[k] = v
			}
		}
	}
	m["status"] = st
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return client.RawPatch(types.MergePatchType, b), nil
}

// Required (non-omitempty) status fields of the CRDs.
var (
	workerPoolStatusRequired = []string{"desired", "launching", "registered", "busy", "idle", "draining"}
	macHostStatusRequired    = []string{"runningVMs"}
)

// AnnFloorOverride holds a temporary floor set through the management API
// (`cucinactl pools floor`, R-SCALE-7): JSON FloorOverride. It is opt-in
// standing cost and always expires.
const AnnFloorOverride = "cucina.sloper.ai/floor-override"

// FloorOverride is the value of AnnFloorOverride.
type FloorOverride struct {
	MinRunning int32     `json:"minRunning"`
	ExpiresAt  time.Time `json:"expiresAt"`
	SetBy      string    `json:"setBy,omitempty"`
}

// ReadFloorOverride parses the annotation (nil when absent or corrupt).
func ReadFloorOverride(wp *v1alpha1.WorkerPool) *FloorOverride {
	s := wp.Annotations[AnnFloorOverride]
	if s == "" {
		return nil
	}
	var f FloorOverride
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return nil
	}
	return &f
}

// SetFloorOverride writes (or with MinRunning 0 removes) the annotation.
func SetFloorOverride(ctx context.Context, c client.Client, ns, pool string, f FloorOverride) error {
	if f.MinRunning <= 0 {
		return patchAnnotation(ctx, c, ns, pool, AnnFloorOverride, "")
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return patchAnnotation(ctx, c, ns, pool, AnnFloorOverride, string(b))
}
