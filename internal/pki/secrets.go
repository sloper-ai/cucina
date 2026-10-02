// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Labels and annotations on Secrets Cucina manages. Secrets without
// LabelManagedCert (an existing Secret, a cert-manager Certificate) are never
// modified.
const (
	LabelManagedBy        = "app.kubernetes.io/managed-by"
	ManagedByValue        = "cucina-controller"
	LabelManagedCert      = "cucina.sloper.ai/certificate" // "ca" | "server" | "controller"
	AnnotationComponent   = "cucina.sloper.ai/component"
	AnnotationRenewReason = "cucina.sloper.ai/last-issue-reason"
)

// CertSpec describes one in-cluster certificate Secret (kubernetes.io/tls with
// tls.crt = leaf + issuing CA, tls.key, ca.crt = trust bundle). The JSON form is
// meant for controller.json / the bootstrap hook.
type CertSpec struct {
	// SecretName is the Secret in the release namespace.
	SecretName string `json:"secretName"`
	// Role is RoleServer (default) or RoleController (the BuildQueueState client
	// certificate, spiffe://cucina/controller, clientAuth only).
	Role Role `json:"role,omitempty"`
	// Component names the server (frontend, storage, scheduler, controller, sts);
	// it becomes spiffe://cucina/server/<component>.
	Component string `json:"component,omitempty"`
	// DNSNames and IPAddresses are the server's SANs (service DNS names,
	// endpoints.serverName, public hostnames).
	DNSNames    []string `json:"dnsNames,omitempty"`
	IPAddresses []string `json:"ipAddresses,omitempty"`
	// ClientAuth adds the clientAuth EKU to a server certificate whose component
	// also dials mTLS (the frontend towards storage shards).
	ClientAuth bool `json:"clientAuth,omitempty"`
	// Lifetime overrides the default (90 days).
	Lifetime config.Duration `json:"lifetime,omitzero"`
}

// ServerCertSpec is the name used by the bootstrap hook.
type ServerCertSpec = CertSpec

func (s CertSpec) role() Role {
	if s.Role == "" {
		return RoleServer
	}
	return s.Role
}

func (s CertSpec) validate() error {
	if s.SecretName == "" {
		return errors.New("pki: certificate spec without secretName")
	}
	switch s.role() {
	case RoleServer:
		if _, err := ServerIdentity(s.Component); err != nil {
			return fmt.Errorf("pki: certificate %s: %w", s.SecretName, err)
		}
		if len(s.DNSNames) == 0 && len(s.IPAddresses) == 0 {
			return fmt.Errorf("pki: server certificate %s needs DNS names or IP addresses", s.SecretName)
		}
		for _, ip := range s.IPAddresses {
			if net.ParseIP(ip) == nil {
				return fmt.Errorf("pki: server certificate %s: invalid IP address %q", s.SecretName, ip)
			}
		}
	case RoleController:
		if len(s.DNSNames) > 0 || len(s.IPAddresses) > 0 || s.Component != "" {
			return fmt.Errorf("pki: controller client certificate %s takes no SANs or component", s.SecretName)
		}
	default:
		return fmt.Errorf("pki: certificate %s: role %q is not server or controller", s.SecretName, s.Role)
	}
	return nil
}

// CertAction is what EnsureCerts did with one Secret.
type CertAction string

const (
	CertCreated   CertAction = "created"
	CertRenewed   CertAction = "renewed"
	CertBundle    CertAction = "bundle-updated" // only ca.crt changed
	CertUnchanged CertAction = "unchanged"
	CertUnmanaged CertAction = "unmanaged" // exists without Cucina's label: never touched
)

// CertResult reports one Secret.
type CertResult struct {
	SecretName string
	Role       Role
	Action     CertAction
	Reason     string
	NotAfter   time.Time
}

// EnsureCA returns the CA stored in the Secret ns/name, creating a new
// self-signed ECDSA P-256 CA if the Secret does not exist. An existing Secret is
// never overwritten, even if it is unusable (that is an error for the operator).
func EnsureCA(ctx context.Context, c client.Client, ns, name string) (*CA, error) {
	return ensureCA(ctx, c, ns, name, time.Now())
}

func ensureCA(ctx context.Context, c client.Client, ns, name string, now time.Time) (*CA, error) {
	var sec corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec)
	switch {
	case err == nil:
		ca, perr := ParseCA(sec.Data)
		if perr != nil {
			return nil, fmt.Errorf("pki: CA Secret %s/%s exists but is unusable (not overwritten): %w", ns, name, perr)
		}
		return ca, nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("pki: reading CA Secret %s/%s: %w", ns, name, err)
	}
	data, err := NewCAMaterial(now, DefaultCAValidity, rand.Reader)
	if err != nil {
		return nil, err
	}
	sec = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{
			LabelManagedBy: ManagedByValue, LabelManagedCert: "ca",
		}},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := c.Create(ctx, &sec); err != nil {
		if apierrors.IsAlreadyExists(err) { // another bootstrap won the race: use its CA
			return ensureCA(ctx, c, ns, name, now)
		}
		return nil, fmt.Errorf("pki: creating CA Secret %s/%s: %w", ns, name, err)
	}
	return ParseCA(data)
}

// LoadCA reads the CA Secret (no creation).
func LoadCA(ctx context.Context, r client.Reader, ns, name string) (*CA, error) {
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec); err != nil {
		return nil, fmt.Errorf("pki: reading CA Secret %s/%s: %w", ns, name, err)
	}
	return ParseCA(sec.Data)
}

// EnsureServerCerts creates the certificate Secrets of specs that do not exist
// and re-issues Cucina-managed ones that are due (2/3 of their lifetime), whose
// SANs/usages differ from the spec, or that were signed by a CA other than the
// active one; it refreshes ca.crt when the bundle changed. Secrets without
// Cucina's label are never modified. Idempotent.
func EnsureServerCerts(ctx context.Context, c client.Client, ns string, ca *CA, specs []ServerCertSpec) ([]CertResult, error) {
	return ensureCerts(ctx, c, ns, ca, specs, systemClock{}, Policy{})
}

func ensureCerts(ctx context.Context, c client.Client, ns string, ca *CA, specs []CertSpec, clock ports.Clock, pol Policy) ([]CertResult, error) {
	iss, err := NewIssuer(ca, clock, pol)
	if err != nil {
		return nil, err
	}
	var out []CertResult
	var errs []error
	for _, spec := range specs {
		res, err := ensureCert(ctx, c, ns, iss, spec, clock.Now())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, res)
	}
	return out, errors.Join(errs...)
}

func ensureCert(ctx context.Context, c client.Client, ns string, iss *Issuer, spec CertSpec, now time.Time) (CertResult, error) {
	res := CertResult{SecretName: spec.SecretName, Role: spec.role()}
	if err := spec.validate(); err != nil {
		return res, err
	}
	var sec corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: spec.SecretName}, &sec)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return res, fmt.Errorf("pki: reading Secret %s/%s: %w", ns, spec.SecretName, err)
	}
	ca := iss.CA()
	reason := "missing"
	if exists {
		if sec.Labels[LabelManagedCert] != string(spec.role()) {
			res.Action, res.Reason = CertUnmanaged, "Secret exists without "+LabelManagedCert+"="+string(spec.role())
			if leaf, _, perr := parseTLSSecret(sec.Data); perr == nil {
				res.NotAfter = leaf.NotAfter
			}
			return res, nil
		}
		reason = renewalReason(sec.Data, spec, ca, now)
		if reason == "" {
			leaf, _, _ := parseTLSSecret(sec.Data)
			res.NotAfter = leaf.NotAfter
			if !bytes.Equal(sec.Data[SecretCACert], ca.BundlePEM()) {
				sec.Data[SecretCACert] = ca.BundlePEM()
				if err := c.Update(ctx, &sec); err != nil {
					return res, fmt.Errorf("pki: updating ca.crt of %s/%s: %w", ns, spec.SecretName, err)
				}
				res.Action = CertBundle
				return res, nil
			}
			res.Action = CertUnchanged
			return res, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return res, err
	}
	issued, err := issueSpec(iss, spec, key.Public())
	if err != nil {
		return res, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return res, err
	}
	data := map[string][]byte{
		corev1.TLSCertKey:       issued.ChainPEM,
		corev1.TLSPrivateKeyKey: keyPEM,
		SecretCACert:            issued.BundlePEM,
	}
	res.NotAfter = issued.NotAfter
	if exists {
		sec.Data = data
		if sec.Annotations == nil {
			sec.Annotations = map[string]string{}
		}
		sec.Annotations[AnnotationRenewReason] = reason
		if err := c.Update(ctx, &sec); err != nil {
			return res, fmt.Errorf("pki: renewing %s/%s: %w", ns, spec.SecretName, err)
		}
		res.Action, res.Reason = CertRenewed, reason
		return res, nil
	}
	sec = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: spec.SecretName,
			Labels:      map[string]string{LabelManagedBy: ManagedByValue, LabelManagedCert: string(spec.role())},
			Annotations: map[string]string{AnnotationComponent: spec.Component, AnnotationRenewReason: reason},
		},
		Type: corev1.SecretTypeTLS,
		Data: data,
	}
	if err := c.Create(ctx, &sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ensureCert(ctx, c, ns, iss, spec, now)
		}
		return res, fmt.Errorf("pki: creating %s/%s: %w", ns, spec.SecretName, err)
	}
	res.Action, res.Reason = CertCreated, reason
	return res, nil
}

func issueSpec(iss *Issuer, spec CertSpec, pub crypto.PublicKey) (*Issued, error) {
	if spec.role() == RoleController {
		return iss.IssueController(pub)
	}
	ips := make([]net.IP, 0, len(spec.IPAddresses))
	for _, s := range spec.IPAddresses {
		ips = append(ips, net.ParseIP(s))
	}
	return iss.IssueServer(pub, spec.Component, spec.DNSNames, ips, spec.ClientAuth, spec.Lifetime.Duration)
}

func parseTLSSecret(data map[string][]byte) (*x509.Certificate, crypto.Signer, error) {
	certs, err := parseCerts(data[corev1.TLSCertKey])
	if err != nil || len(certs) == 0 {
		return nil, nil, fmt.Errorf("tls.crt: no certificate")
	}
	key, err := parseKey(data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, nil, fmt.Errorf("tls.key: %w", err)
	}
	return certs[0], key, nil
}

// renewalReason returns why a managed Secret must be re-issued, or "".
func renewalReason(data map[string][]byte, spec CertSpec, ca *CA, now time.Time) string {
	leaf, key, err := parseTLSSecret(data)
	if err != nil {
		return "unparsable: " + err.Error()
	}
	if !publicKeysEqual(leaf.PublicKey, key.Public()) {
		return "key does not match certificate"
	}
	if leaf.CheckSignatureFrom(ca.Certificate()) != nil {
		return "not signed by the active CA"
	}
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	if !now.Before(leaf.NotBefore.Add(life * 2 / 3)) {
		return "renewal due (2/3 of lifetime)"
	}
	want := ControllerIdentity()
	wantEKU := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if spec.role() == RoleServer {
		want, _ = ServerIdentity(spec.Component)
		wantEKU = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		if spec.ClientAuth {
			wantEKU = append(wantEKU, x509.ExtKeyUsageClientAuth)
		}
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != want.String() {
		return "identity changed"
	}
	if !slices.Equal(leaf.ExtKeyUsage, wantEKU) {
		return "key usage changed"
	}
	if !sameStrings(leaf.DNSNames, spec.DNSNames) {
		return "DNS names changed"
	}
	var gotIPs []string
	for _, ip := range leaf.IPAddresses {
		gotIPs = append(gotIPs, ip.String())
	}
	var wantIPs []string
	for _, s := range spec.IPAddresses {
		wantIPs = append(wantIPs, net.ParseIP(s).String())
	}
	if !sameStrings(gotIPs, wantIPs) {
		return "IP addresses changed"
	}
	if spec.Lifetime.Duration != 0 && life > spec.Lifetime.Duration+2*Backdate {
		return "lifetime shortened"
	}
	return ""
}

func sameStrings(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// ------------------------------------------------------------- CA rotation

// RotationPhase is one step of the two-root CA rotation (docs/security.md §PKI).
type RotationPhase string

const (
	// RotationIntroduce generates the next CA and adds it to the bundle; the
	// current CA keeps signing. Wait until every verifier trusts both roots.
	RotationIntroduce RotationPhase = "introduce"
	// RotationActivate makes the next CA the signer; both roots stay trusted. The
	// Rotator then re-issues every managed server certificate.
	RotationActivate RotationPhase = "activate"
	// RotationRetire removes every root except the active one, once no
	// certificate signed by the old CA is in use any more.
	RotationRetire RotationPhase = "retire"
)

// RotateCAData applies a rotation phase to CA Secret data (pure; see RotateCA).
func RotateCAData(data map[string][]byte, phase RotationPhase, now time.Time) (map[string][]byte, error) {
	if len(data[SecretCAKey]) == 0 {
		return nil, fmt.Errorf("pki: CA rotation needs a Cucina-managed CA (%s); rotate cert-manager CAs with cert-manager", SecretCAKey)
	}
	cur, err := ParseCA(data)
	if err != nil {
		return nil, err
	}
	out := maps.Clone(data)
	switch phase {
	case RotationIntroduce:
		if len(data[SecretNextCAKey]) > 0 {
			return nil, errors.New("pki: a next CA is already introduced; activate it first")
		}
		if len(cur.roots) > 1 {
			return nil, errors.New("pki: the bundle still holds a retired root; run the retire phase first")
		}
		next, err := NewCAMaterial(now, DefaultCAValidity, rand.Reader)
		if err != nil {
			return nil, err
		}
		out[SecretCACert] = append(append([]byte(nil), cur.BundlePEM()...), next[SecretCACert]...)
		out[SecretNextCAKey] = next[SecretCAKey]
	case RotationActivate:
		if len(data[SecretNextCAKey]) == 0 {
			return nil, errors.New("pki: no next CA introduced")
		}
		out[SecretCAKey] = data[SecretNextCAKey]
		delete(out, SecretNextCAKey)
	case RotationRetire:
		if len(data[SecretNextCAKey]) > 0 {
			return nil, errors.New("pki: a next CA is introduced but not active; activate it first")
		}
		out[SecretCACert] = encodeCerts([]*x509.Certificate{cur.cert})
	default:
		return nil, fmt.Errorf("pki: unknown rotation phase %q", phase)
	}
	if _, err := ParseCA(out); err != nil {
		return nil, fmt.Errorf("pki: rotation phase %s produced an unusable CA: %w", phase, err)
	}
	return out, nil
}

// RotateCA applies a rotation phase to the CA Secret (optimistic concurrency).
func RotateCA(ctx context.Context, c client.Client, ns, name string, phase RotationPhase) error {
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec); err != nil {
		return err
	}
	data, err := RotateCAData(sec.Data, phase, time.Now())
	if err != nil {
		return err
	}
	sec.Data = data
	return c.Update(ctx, &sec)
}

// ------------------------------------------------------------- CA source

// SecretCASource serves the CA from its Secret and re-reads it periodically,
// so rotation phases reach every controller replica without a restart. Pass an
// uncached reader (manager.GetAPIReader()).
type SecretCASource struct {
	reader   client.Reader
	ns, name string
	clock    ports.Clock
	interval time.Duration
	logger   *slog.Logger
	expiry   *ExpiryTracker
	cur      atomic.Pointer[CA]
}

// NewSecretCASource loads the CA once (an error is fatal: fail fast).
func NewSecretCASource(ctx context.Context, r client.Reader, ns, name string, clock ports.Clock, logger *slog.Logger, expiry *ExpiryTracker) (*SecretCASource, error) {
	if logger == nil {
		logger = slog.Default()
	}
	s := &SecretCASource{reader: r, ns: ns, name: name, clock: clock, interval: time.Minute, logger: logger, expiry: expiry}
	if err := s.Refresh(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Current implements CASource.
func (s *SecretCASource) Current() *CA { return s.cur.Load() }

// Refresh re-reads the Secret; on error the previous CA stays in use.
func (s *SecretCASource) Refresh(ctx context.Context) error {
	ca, err := LoadCA(ctx, s.reader, s.ns, s.name)
	if err != nil {
		return err
	}
	s.cur.Store(ca)
	for i, r := range ca.roots {
		s.expiry.Set(ExpiryRoleCA, fmt.Sprintf("root-%d", i), r.NotAfter)
	}
	s.expiry.Set(ExpiryRoleCA, "issuer", ca.cert.NotAfter)
	return nil
}

// Start implements manager.Runnable (every replica).
func (s *SecretCASource) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.clock.After(s.interval):
			if err := s.Refresh(ctx); err != nil {
				s.logger.Warn("CA Secret refresh failed; keeping the previous CA", "secret", s.ns+"/"+s.name, "error", err)
			}
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every replica issues.
func (s *SecretCASource) NeedLeaderElection() bool { return false }

// systemClock is the wall clock for one-shot tools (bootstrap).
type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
