// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/keys"
)

// Signing-key operations around internal/keys (R-AUTH-9, T10e): the rotation
// probe (LoadVerifier), the compromise restart (FrontendRestarter), the
// Cloud Identity group resolver, and `cucina-controller keys rotate|compromise`.

// Chart labels of the Buildbarn components that validate Cucina JWTs.
const (
	labelInstance  = "app.kubernetes.io/instance"
	labelComponent = "app.kubernetes.io/component"
	clientPortName = "grpc-client"
)

var jwtValidators = []string{"frontend", "scheduler"}

// frontendProbe implements keys.LoadVerifier: a pending key counts as loaded
// once every ready frontend and scheduler Pod authenticates a short-lived,
// grant-less probe token signed with it (UNAUTHENTICATED means not yet).
type frontendProbe struct {
	c        client.Reader
	ns       string
	release  string
	minter   *keys.Minter
	ca       string // CA bundle file: the probe verifies the chain, not the name (Pod IPs)
	instance string
	log      *slog.Logger
}

// KeyLoaded implements keys.LoadVerifier.
func (p *frontendProbe) KeyLoaded(ctx context.Context, kid string) (bool, error) {
	tok, _, err := p.minter.MintWithKey(kid, keys.MintRequest{Subject: "cucina:key-rotation-probe", Scopes: keys.Scopes{}.Normalized(), TTL: time.Minute})
	if err != nil {
		return false, err
	}
	pem, err := os.ReadFile(p.ca)
	if err != nil {
		return false, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return false, errors.New("key probe: CA bundle has no certificates")
	}
	targets := 0
	for _, comp := range jwtValidators {
		var pods corev1.PodList
		if err := p.c.List(ctx, &pods, client.InNamespace(p.ns), client.MatchingLabels{labelInstance: p.release, labelComponent: comp}); err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			addr := podClientAddr(&pod)
			if addr == "" {
				continue
			}
			targets++
			ok, err := p.probe(ctx, addr, roots, tok)
			if err != nil || !ok {
				p.log.Info("key not loaded yet", "kid", kid, "pod", pod.Name, "err", err)
				return false, nil
			}
		}
	}
	if targets == 0 {
		return false, errors.New("key probe: no ready frontend or scheduler Pods found")
	}
	return true, nil
}

func podClientAddr(pod *corev1.Pod) string {
	if pod.Status.PodIP == "" || pod.DeletionTimestamp != nil {
		return ""
	}
	ready := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return ""
	}
	for _, c := range pod.Spec.Containers {
		for _, port := range c.Ports {
			if port.Name == clientPortName {
				return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port.ContainerPort)))
			}
		}
	}
	return ""
}

func (p *frontendProbe) probe(ctx context.Context, addr string, roots *x509.CertPool, tok string) (bool, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // chain verified below; Pod IPs are not in the SANs
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no peer certificate")
			}
			opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
			for _, c := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			return err
		},
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	cctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok), 5*time.Second)
	defer cancel()
	_, err = repb.NewCapabilitiesClient(conn).GetCapabilities(cctx, &repb.GetCapabilitiesRequest{InstanceName: p.instance})
	switch status.Code(err) {
	case codes.OK, codes.PermissionDenied:
		return true, nil // authenticated: the key is loaded (the probe has no grants)
	case codes.Unauthenticated:
		return false, nil
	default:
		return false, err
	}
}

// deploymentRestarter implements keys.FrontendRestarter: a rolling restart of
// the frontend and scheduler Deployments (kubectl rollout restart), so they
// drop cached validations of tokens signed by a compromised key.
type deploymentRestarter struct {
	c       client.Client
	ns      string
	release string
	clock   func() time.Time
	log     *slog.Logger
}

// RestartFrontends implements keys.FrontendRestarter.
func (r *deploymentRestarter) RestartFrontends(ctx context.Context, reason string) error {
	var errs []error
	for _, comp := range jwtValidators {
		var deps appsv1.DeploymentList
		if err := r.c.List(ctx, &deps, client.InNamespace(r.ns), client.MatchingLabels{labelInstance: r.release, labelComponent: comp}); err != nil {
			errs = append(errs, err)
			continue
		}
		for i := range deps.Items {
			patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
				"annotations": map[string]string{"kubectl.kubernetes.io/restartedAt": r.clock().UTC().Format(time.RFC3339), "cucina.sloper.ai/restart-reason": reason},
			}}}})
			if err := r.c.Patch(ctx, &deps.Items[i], client.RawPatch(types.MergePatchType, patch)); err != nil {
				errs = append(errs, err)
				continue
			}
			r.log.Warn("restarted JWT validators", "deployment", deps.Items[i].Name, "reason", reason)
		}
	}
	return errors.Join(errs...)
}

// groupResolver builds the optional Cloud Identity group resolver
// (auth.groupLookup.serviceAccountSecret: a Google service-account JSON key).
func groupResolver(ctx context.Context, cs kubernetes.Interface, cfg *config.Controller, d *Deps) (auth.GroupResolver, error) {
	gl := cfg.Auth.GroupLookup
	if gl == nil || gl.ServiceAccountSecret == "" {
		return nil, nil
	}
	sec, err := cs.CoreV1().Secrets(cfg.Namespace).Get(ctx, gl.ServiceAccountSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("group lookup Secret %s: %w", gl.ServiceAccountSecret, err)
	}
	var key []byte
	for _, name := range []string{"key.json", "credentials.json", "service-account.json"} {
		if v, ok := sec.Data[name]; ok {
			key = v
			break
		}
	}
	if key == nil && len(sec.Data) == 1 {
		for _, v := range sec.Data {
			key = v
		}
	}
	if key == nil {
		return nil, fmt.Errorf("group lookup Secret %s: expected one entry key.json", gl.ServiceAccountSecret)
	}
	backend, err := auth.NewCloudIdentity(ctx, key)
	if err != nil {
		return nil, err
	}
	return auth.NewCachedGroups(backend, d.Clock, 10000), nil
}

// KeysCommand runs `cucina-controller keys rotate|compromise` against the
// signing-key Secret. rotate publishes a successor key; the leader promotes it
// once every frontend and scheduler loaded it (keys.LoadVerifier). compromise
// removes kid ("*" = all) from the JWKS at once, activates a fresh key and
// restarts the frontends and the scheduler.
func KeysCommand(ctx context.Context, cfg *config.Controller, action, kid, reason string, log *slog.Logger) (string, error) {
	rc, err := ctrl.GetConfig()
	if err != nil {
		return "", err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return "", err
	}
	scheme, err := NewScheme()
	if err != nil {
		return "", err
	}
	cl, err := client.New(rc, client.Options{Scheme: scheme})
	if err != nil {
		return "", err
	}
	m, err := keys.NewManager(keys.NewKubeObjects(cs, cfg.Namespace), cfg.Auth, cfg.Endpoints.STSURL, keys.Options{
		Log:       log,
		Restarter: &deploymentRestarter{c: cl, ns: cfg.Namespace, release: cfg.ReleaseName, clock: time.Now, log: log},
	})
	if err != nil {
		return "", err
	}
	switch action {
	case "rotate":
		return m.Rotator.StartRotation(ctx)
	case "compromise":
		if kid == "" {
			return "", errors.New(`compromise needs --kid (or "*" for every key)`)
		}
		return "", m.Rotator.Compromise(ctx, kid, reason)
	}
	return "", fmt.Errorf("unknown keys action %q", action)
}
