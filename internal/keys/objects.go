// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Objects is the narrow Kubernetes surface this package needs: Secrets and ConfigMaps
// of one namespace, read directly from the API server (never from an informer cache,
// so that revocations are seen at once). Updates use optimistic concurrency: an update
// carrying a stale ResourceVersion fails with ErrConflict.
type Objects interface {
	GetSecret(ctx context.Context, name string) (*corev1.Secret, error)
	CreateSecret(ctx context.Context, s *corev1.Secret) (*corev1.Secret, error)
	UpdateSecret(ctx context.Context, s *corev1.Secret) (*corev1.Secret, error)
	GetConfigMap(ctx context.Context, name string) (*corev1.ConfigMap, error)
	CreateConfigMap(ctx context.Context, c *corev1.ConfigMap) (*corev1.ConfigMap, error)
	UpdateConfigMap(ctx context.Context, c *corev1.ConfigMap) (*corev1.ConfigMap, error)
}

// Errors returned by Objects implementations.
var (
	ErrNotFound      = errors.New("object not found")
	ErrAlreadyExists = errors.New("object already exists")
	ErrConflict      = errors.New("object was modified concurrently")
)

// Labels put on every object this package creates (they are not Helm-managed).
var managedLabels = map[string]string{
	"app.kubernetes.io/managed-by": "cucina-controller",
	"app.kubernetes.io/part-of":    "cucina",
}

// NewKubeObjects adapts a client-go clientset.
func NewKubeObjects(cs kubernetes.Interface, namespace string) Objects {
	return kubeObjects{cs: cs, ns: namespace}
}

type kubeObjects struct {
	cs kubernetes.Interface
	ns string
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return errors.Join(ErrNotFound, err)
	case apierrors.IsAlreadyExists(err):
		return errors.Join(ErrAlreadyExists, err)
	case apierrors.IsConflict(err):
		return errors.Join(ErrConflict, err)
	}
	return err
}

func (k kubeObjects) GetSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	s, err := k.cs.CoreV1().Secrets(k.ns).Get(ctx, name, metav1.GetOptions{})
	return s, mapErr(err)
}

func (k kubeObjects) CreateSecret(ctx context.Context, s *corev1.Secret) (*corev1.Secret, error) {
	s = s.DeepCopy()
	s.Namespace = k.ns
	out, err := k.cs.CoreV1().Secrets(k.ns).Create(ctx, s, metav1.CreateOptions{})
	return out, mapErr(err)
}

func (k kubeObjects) UpdateSecret(ctx context.Context, s *corev1.Secret) (*corev1.Secret, error) {
	out, err := k.cs.CoreV1().Secrets(k.ns).Update(ctx, s, metav1.UpdateOptions{})
	return out, mapErr(err)
}

func (k kubeObjects) GetConfigMap(ctx context.Context, name string) (*corev1.ConfigMap, error) {
	c, err := k.cs.CoreV1().ConfigMaps(k.ns).Get(ctx, name, metav1.GetOptions{})
	return c, mapErr(err)
}

func (k kubeObjects) CreateConfigMap(ctx context.Context, c *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	c = c.DeepCopy()
	c.Namespace = k.ns
	out, err := k.cs.CoreV1().ConfigMaps(k.ns).Create(ctx, c, metav1.CreateOptions{})
	return out, mapErr(err)
}

func (k kubeObjects) UpdateConfigMap(ctx context.Context, c *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	out, err := k.cs.CoreV1().ConfigMaps(k.ns).Update(ctx, c, metav1.UpdateOptions{})
	return out, mapErr(err)
}

func newSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: copyLabels()},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

func newConfigMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: copyLabels()}, Data: data}
}

func copyLabels() map[string]string {
	out := make(map[string]string, len(managedLabels))
	for k, v := range managedLabels {
		out[k] = v
	}
	return out
}

// updateSecret applies mutate to the latest version of a Secret and writes it back,
// retrying on conflicts. mutate returns false when nothing needs to change.
func updateSecret(ctx context.Context, o Objects, name string, mutate func(*corev1.Secret) (bool, error)) (*corev1.Secret, error) {
	for attempt := 0; ; attempt++ {
		s, err := o.GetSecret(ctx, name)
		if err != nil {
			return nil, err
		}
		s = s.DeepCopy()
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		changed, err := mutate(s)
		if err != nil || !changed {
			return s, err
		}
		out, err := o.UpdateSecret(ctx, s)
		if errors.Is(err, ErrConflict) && attempt < 8 {
			continue
		}
		return out, err
	}
}

// updateConfigMap is updateSecret for ConfigMaps.
func updateConfigMap(ctx context.Context, o Objects, name string, mutate func(*corev1.ConfigMap) (bool, error)) (*corev1.ConfigMap, error) {
	for attempt := 0; ; attempt++ {
		c, err := o.GetConfigMap(ctx, name)
		if err != nil {
			return nil, err
		}
		c = c.DeepCopy()
		if c.Data == nil {
			c.Data = map[string]string{}
		}
		changed, err := mutate(c)
		if err != nil || !changed {
			return c, err
		}
		out, err := o.UpdateConfigMap(ctx, c)
		if errors.Is(err, ErrConflict) && attempt < 8 {
			continue
		}
		return out, err
	}
}
