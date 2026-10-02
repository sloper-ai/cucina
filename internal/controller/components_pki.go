// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"

	"github.com/sloper-ai/cucina/internal/pki"
)

// Cucina's private CA (agent `enroll`): the issuer of worker, VM, host and
// server certificates, served from its Secret on every replica, the
// leader-only renewal of the in-cluster server certificates (--certs), the
// cucina_cert_expiry_seconds collector, and the bootstrap hook steps.
func init() {
	RegisterComponent(Factory{Name: "pki", Modes: []Mode{ModeController}, Order: -40, New: newPKI})
	RegisterBootstrapStep(BootstrapStep{Name: "pki-ca", Order: 10, Run: bootstrapCA})
	RegisterBootstrapStep(BootstrapStep{Name: "pki-server-certs", Order: 20, Run: bootstrapServerCerts})
}

// Shared object keys.
const (
	SharedPKIIssuer = "pki.issuer"
	SharedPKIExpiry = "pki.expiry"
)

func newPKI(ctx context.Context, d *Deps) (any, error) {
	cfg := d.Config
	expiry := pki.NewExpiryTracker(d.Clock)
	if err := d.Registry.Register(expiry); err != nil {
		return nil, fmt.Errorf("registering %s: %w", pki.ExpiryMetricName, err)
	}
	src, err := pki.NewSecretCASource(ctx, d.APIReader, cfg.Namespace, cfg.PKI.CASecret, d.Clock, d.Log.With("component", "pki"), expiry)
	if err != nil {
		return nil, fmt.Errorf("loading the CA from Secret %s (did the bootstrap hook run?): %w", cfg.PKI.CASecret, err)
	}
	if err := d.Manager.Add(src); err != nil {
		return nil, err
	}
	issuer, err := pki.NewIssuer(src, d.Clock, pki.PolicyFromConfig(cfg.PKI))
	if err != nil {
		return nil, err
	}
	d.Share(SharedPKIIssuer, issuer)
	d.Share(SharedPKIExpiry, expiry)
	if d.CertsFile != "" {
		specs, err := loadCertSpecs(d.CertsFile)
		if err != nil {
			return nil, err
		}
		rot := &pki.Rotator{Client: d.Client, Namespace: cfg.Namespace, CASecret: cfg.PKI.CASecret, Specs: specs,
			Clock: d.Clock, Policy: pki.PolicyFromConfig(cfg.PKI), Expiry: expiry, Logger: d.Log.With("component", "pki-rotator")}
		if err := d.Manager.Add(rot); err != nil {
			return nil, err
		}
	}
	return struct{}{}, nil
}

// loadCertSpecs parses the certificate list strictly.
func loadCertSpecs(path string) ([]pki.CertSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("certificate list: %w", err)
	}
	var specs []pki.CertSpec
	if err := json.Unmarshal(b, &specs, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("certificate list %s: %w", path, err)
	}
	return specs, nil
}

func caSecretName(bd *BootstrapDeps) string {
	if bd.Config != nil && bd.Config.PKI.CASecret != "" {
		return bd.Config.PKI.CASecret
	}
	return bd.Release + "-ca"
}

func bootstrapCA(ctx context.Context, bd *BootstrapDeps) error {
	if _, err := pki.EnsureCA(ctx, bd.Client, bd.Namespace, caSecretName(bd)); err != nil {
		return err
	}
	bd.Log.Info("CA ready", "secret", caSecretName(bd))
	return nil
}

func bootstrapServerCerts(ctx context.Context, bd *BootstrapDeps) error {
	if bd.CertsFile == "" {
		bd.Log.Info("no --certs list: server certificates come from cert-manager or existing Secrets")
		return nil
	}
	specs, err := loadCertSpecs(bd.CertsFile)
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		return errors.New("certificate list is empty")
	}
	ca, err := pki.LoadCA(ctx, bd.Client, bd.Namespace, caSecretName(bd))
	if err != nil {
		return err
	}
	results, err := pki.EnsureServerCerts(ctx, bd.Client, bd.Namespace, ca, specs)
	for _, r := range results {
		bd.Log.Info("certificate", "secret", r.SecretName, "role", string(r.Role), "action", string(r.Action), "notAfter", r.NotAfter)
	}
	return err
}
