// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/pki"
)

// Kubernetes representation (documented in docs/security.md §Enrollment).
const (
	LabelManagedBy        = "app.kubernetes.io/managed-by"
	ManagedByValue        = "cucina-controller"
	LabelEnrollToken      = "cucina.sloper.ai/enroll-token"        // MacHost: id of the token that bound the host
	LabelEnrollment       = "cucina.sloper.ai/enrollment"          // Lease: "worker-launch"
	AnnotationIdentityKey = "cucina.sloper.ai/identity-key-sha256" // MacHost: bound key (pending or enrolled)
	AnnotationPending     = "cucina.sloper.ai/pending-since"
	AnnotationEnrolled    = "cucina.sloper.ai/enrolled-at"
	AnnotationHostname    = "cucina.sloper.ai/hostname"
	AnnotationLaunch      = "cucina.sloper.ai/launch-record"
	conflictRetries       = 8
)

// ------------------------------------------------------------ token Secret

// SecretTokens stores token records in one Secret (one data key per token,
// "<id>.json"); writes use optimistic concurrency, so every controller replica
// sees one consistent host count per token. Pass an uncached reader.
type SecretTokens struct {
	Reader    client.Reader
	Client    client.Client
	Namespace string
	Name      string
}

func (s *SecretTokens) get(ctx context.Context) (*corev1.Secret, error) {
	var sec corev1.Secret
	err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &sec)
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	return &sec, err
}

func tokenKey(id string) string { return id + ".json" }

// Get implements TokenStore.
func (s *SecretTokens) Get(ctx context.Context, id string) (TokenRecord, error) {
	sec, err := s.get(ctx)
	if err != nil {
		return TokenRecord{}, err
	}
	raw, ok := sec.Data[tokenKey(id)]
	if !ok {
		return TokenRecord{}, ErrNotFound
	}
	var r TokenRecord
	return r, json.Unmarshal(raw, &r)
}

// List implements TokenStore.
func (s *SecretTokens) List(ctx context.Context) ([]TokenRecord, error) {
	sec, err := s.get(ctx)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []TokenRecord
	for k, raw := range sec.Data {
		if !strings.HasSuffix(k, ".json") {
			continue
		}
		var r TokenRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("enroll: token %s: %w", k, err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Create implements TokenStore.
func (s *SecretTokens) Create(ctx context.Context, rec TokenRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	for range conflictRetries {
		sec, err := s.get(ctx)
		if errors.Is(err, ErrNotFound) {
			sec = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name, Labels: map[string]string{LabelManagedBy: ManagedByValue}},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{tokenKey(rec.ID): raw},
			}
			if err := s.Client.Create(ctx, sec); apierrors.IsAlreadyExists(err) {
				continue
			} else {
				return err
			}
		}
		if err != nil {
			return err
		}
		if _, dup := sec.Data[tokenKey(rec.ID)]; dup {
			return ErrExists
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[tokenKey(rec.ID)] = raw
		if err := s.Client.Update(ctx, sec); apierrors.IsConflict(err) {
			continue
		} else {
			return err
		}
	}
	return errors.New("enroll: token store: too many write conflicts")
}

// Update implements TokenStore.
func (s *SecretTokens) Update(ctx context.Context, id string, fn func(*TokenRecord) error) (TokenRecord, error) {
	for range conflictRetries {
		sec, err := s.get(ctx)
		if err != nil {
			return TokenRecord{}, err
		}
		raw, ok := sec.Data[tokenKey(id)]
		if !ok {
			return TokenRecord{}, ErrNotFound
		}
		var r TokenRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return TokenRecord{}, err
		}
		if err := fn(&r); err != nil {
			return TokenRecord{}, err
		}
		if sec.Data[tokenKey(id)], err = json.Marshal(r); err != nil {
			return TokenRecord{}, err
		}
		if err := s.Client.Update(ctx, sec); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return TokenRecord{}, err
		}
		return r, nil
	}
	return TokenRecord{}, errors.New("enroll: token store: too many write conflicts")
}

// ----------------------------------------------------------- MacHost store

// MacHostStore maps HostRecords onto MacHost objects (name = lower-case
// serial for objects Cucina creates; objects with other names are found by
// spec.serial). Enrollment state lives in labels/annotations; the certificate
// expiry in status.certificateExpiry. Pass an uncached reader.
type MacHostStore struct {
	Reader    client.Reader
	Client    client.Client
	Namespace string
}

func (s *MacHostStore) find(ctx context.Context, serial string) (*v1alpha1.MacHost, error) {
	var mh v1alpha1.MacHost
	err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: strings.ToLower(serial)}, &mh)
	if err == nil && canonical(mh.Spec.Serial) == serial {
		return &mh, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	var list v1alpha1.MacHostList
	if err := s.Reader.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if canonical(list.Items[i].Spec.Serial) == serial {
			return &list.Items[i], nil
		}
	}
	return nil, ErrNotFound
}

func canonical(serial string) string {
	c, err := pki.CanonicalSerial(serial)
	if err != nil {
		return ""
	}
	return c
}

// Get implements HostStore.
func (s *MacHostStore) Get(ctx context.Context, serial string) (HostRecord, error) {
	mh, err := s.find(ctx, serial)
	if err != nil {
		return HostRecord{}, err
	}
	return toRecord(mh), nil
}

// List implements HostStore.
func (s *MacHostStore) List(ctx context.Context) ([]HostRecord, error) {
	var list v1alpha1.MacHostList
	if err := s.Reader.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	out := make([]HostRecord, 0, len(list.Items))
	for i := range list.Items {
		if canonical(list.Items[i].Spec.Serial) != "" {
			out = append(out, toRecord(&list.Items[i]))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out, nil
}

// Create implements HostStore.
func (s *MacHostStore) Create(ctx context.Context, rec HostRecord) error {
	if _, err := s.find(ctx, rec.Serial); err == nil {
		return ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	mh := &v1alpha1.MacHost{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: strings.ToLower(rec.Serial)}}
	apply(mh, rec)
	if err := s.Client.Create(ctx, mh); apierrors.IsAlreadyExists(err) {
		return ErrExists
	} else if err != nil {
		return err
	}
	return s.patchStatus(ctx, mh, rec)
}

// Update implements HostStore.
func (s *MacHostStore) Update(ctx context.Context, serial string, fn func(*HostRecord) error) (HostRecord, error) {
	for range conflictRetries {
		mh, err := s.find(ctx, serial)
		if err != nil {
			return HostRecord{}, err
		}
		rec := toRecord(mh)
		if err := fn(&rec); err != nil {
			return HostRecord{}, err
		}
		rec.Serial, rec.Name = serial, mh.Name
		apply(mh, rec)
		if err := s.Client.Update(ctx, mh); apierrors.IsConflict(err) {
			continue
		} else if apierrors.IsNotFound(err) {
			return HostRecord{}, ErrNotFound
		} else if err != nil {
			return HostRecord{}, err
		}
		return rec, s.patchStatus(ctx, mh, rec)
	}
	return HostRecord{}, errors.New("enroll: MacHost store: too many write conflicts")
}

// Delete implements HostStore.
func (s *MacHostStore) Delete(ctx context.Context, serial string) error {
	mh, err := s.find(ctx, serial)
	if err != nil {
		return err
	}
	if err := s.Client.Delete(ctx, mh); apierrors.IsNotFound(err) {
		return ErrNotFound
	} else {
		return err
	}
}

// patchStatus records the certificate expiry (status is otherwise owned by the
// MacHost reconciler; the merge patch touches only this field).
func (s *MacHostStore) patchStatus(ctx context.Context, mh *v1alpha1.MacHost, rec HostRecord) error {
	if rec.CertExpiry.IsZero() {
		return nil
	}
	if mh.Status.CertificateExpiry != nil && mh.Status.CertificateExpiry.Time.Equal(rec.CertExpiry) {
		return nil
	}
	orig := mh.DeepCopy()
	t := metav1.NewTime(rec.CertExpiry)
	mh.Status.CertificateExpiry = &t
	return s.Client.Status().Patch(ctx, mh, client.MergeFrom(orig))
}

func toRecord(mh *v1alpha1.MacHost) HostRecord {
	a := mh.Annotations
	rec := HostRecord{
		Serial:       canonical(mh.Spec.Serial),
		Name:         mh.Name,
		Site:         mh.Spec.Site,
		Labels:       maps.Clone(mh.Spec.Labels),
		Approved:     mh.Spec.Approved,
		TokenID:      mh.Labels[LabelEnrollToken],
		KeySHA256:    a[AnnotationIdentityKey],
		PendingSince: parseTime(a[AnnotationPending]),
		EnrolledAt:   parseTime(a[AnnotationEnrolled]),
		Hostname:     a[AnnotationHostname],
	}
	if mh.Status.CertificateExpiry != nil {
		rec.CertExpiry = mh.Status.CertificateExpiry.Time
	}
	return rec
}

func apply(mh *v1alpha1.MacHost, rec HostRecord) {
	mh.Spec.Serial = rec.Serial
	mh.Spec.Site = rec.Site
	mh.Spec.Labels = maps.Clone(rec.Labels)
	mh.Spec.Approved = rec.Approved
	if mh.Labels == nil {
		mh.Labels = map[string]string{}
	}
	if mh.Annotations == nil {
		mh.Annotations = map[string]string{}
	}
	mh.Labels[LabelManagedBy] = ManagedByValue
	setOrDelete(mh.Labels, LabelEnrollToken, rec.TokenID)
	setOrDelete(mh.Annotations, AnnotationIdentityKey, rec.KeySHA256)
	setOrDelete(mh.Annotations, AnnotationPending, formatTime(rec.PendingSince))
	setOrDelete(mh.Annotations, AnnotationEnrolled, formatTime(rec.EnrolledAt))
	setOrDelete(mh.Annotations, AnnotationHostname, truncate(rec.Hostname, 253))
}

func setOrDelete(m map[string]string, k, v string) {
	if v == "" {
		delete(m, k)
		return
	}
	m[k] = v
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ------------------------------------------------------------ launch Leases

// LeaseReplay stores one coordination.k8s.io Lease per enrolled EC2 instance
// ("cucina-enroll-<instance-id>"): creation is atomic across controller
// replicas, so a launch's first enrollment wins everywhere. Pass an uncached reader.
type LeaseReplay struct {
	Reader    client.Reader
	Client    client.Client
	Namespace string
}

func leaseName(instanceID string) string { return "cucina-enroll-" + instanceID }

// Claim implements ReplayStore.
func (s *LeaseReplay) Claim(ctx context.Context, instanceID string, fn func(*LaunchRecord, bool) error) error {
	for range conflictRetries {
		var lease coordinationv1.Lease
		err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: leaseName(instanceID)}, &lease)
		found := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		var rec LaunchRecord
		if found {
			if err := json.Unmarshal([]byte(lease.Annotations[AnnotationLaunch]), &rec); err != nil {
				return fmt.Errorf("enroll: launch record of %s: %w", instanceID, err)
			}
		}
		if err := fn(&rec, found); err != nil {
			return err
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		acquired := metav1.NewMicroTime(rec.FirstIssued)
		if !found {
			lease = coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
				Namespace: s.Namespace, Name: leaseName(instanceID),
				Labels: map[string]string{LabelManagedBy: ManagedByValue, LabelEnrollment: "worker-launch"},
			}}
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		lease.Annotations[AnnotationLaunch] = string(raw)
		holder := instanceID
		lease.Spec.HolderIdentity, lease.Spec.AcquireTime = &holder, &acquired
		if !found {
			err = s.Client.Create(ctx, &lease)
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return err
		}
		if err := s.Client.Update(ctx, &lease); apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			continue
		} else {
			return err
		}
	}
	return errors.New("enroll: launch records: too many write conflicts")
}

// Prune implements ReplayStore.
func (s *LeaseReplay) Prune(ctx context.Context, cutoff time.Time) (int, error) {
	var list coordinationv1.LeaseList
	if err := s.Reader.List(ctx, &list, client.InNamespace(s.Namespace), client.MatchingLabels{LabelEnrollment: "worker-launch"}); err != nil {
		return 0, err
	}
	n := 0
	for i := range list.Items {
		l := &list.Items[i]
		if l.Spec.AcquireTime != nil && l.Spec.AcquireTime.Time.Before(cutoff) {
			if err := s.Client.Delete(ctx, l); err != nil && !apierrors.IsNotFound(err) {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
