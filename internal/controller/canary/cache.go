// SPDX-License-Identifier: FSL-1.1-ALv2

// Package canary implements `cucina-controller canary cache`: the synthetic
// cache canary (R-TEST-7) used by `helm test` (R-CP-7) and the canary CronJob.
// It mints a Cucina JWT at the STS with a service-account key (break-glass or
// a dedicated canary key), then through the client endpoint calls
// GetCapabilities, writes and reads a unique CAS blob, and writes and reads an
// AC entry that references it. It starts no workers.
package canary

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// Subject token types the STS accepts for service-account keys (R-AUTH-10).
const (
	tokenExchangeGrant  = "urn:ietf:params:oauth:grant-type:token-exchange"
	serviceKeyTokenType = "urn:cucina:params:oauth:token-type:service-key"
)

// Options configure the cache canary.
type Options struct {
	// STSURL is the STS base URL (https://…/); the token endpoint is <STSURL>/token.
	STSURL string
	// Endpoint is the client endpoint: grpcs://host:port or host:port.
	Endpoint string
	// ServerName overrides the TLS server name.
	ServerName string
	// InstanceName is the Buildbarn instance name (default "main").
	InstanceName string
	// CAFile verifies the STS and the endpoint (empty: system roots).
	CAFile string
	// KeyFile holds the service-account key (never logged).
	KeyFile string
	// Token is used instead of an STS exchange when set (tests).
	Token string
	// DialOptions are extra gRPC dial options (tests).
	DialOptions []grpc.DialOption
	// HTTPClient overrides the STS client (tests).
	HTTPClient *http.Client
}

// Step is one timed canary step.
type Step struct {
	Name     string  `json:"name"`
	Seconds  float64 `json:"seconds"`
	Error    string  `json:"error,omitempty"`
	Detailed string  `json:"detail,omitempty"`
}

// Report is printed as JSON by the subcommand.
type Report struct {
	OK    bool   `json:"ok"`
	Steps []Step `json:"steps"`
}

// RunCache runs the cache canary. The report lists every step; err is non-nil
// when any step failed.
func RunCache(ctx context.Context, o Options) (Report, error) {
	if o.InstanceName == "" {
		o.InstanceName = "main"
	}
	rep := Report{}
	step := func(name string, f func() (string, error)) error {
		start := time.Now()
		detail, err := f()
		s := Step{Name: name, Seconds: time.Since(start).Seconds(), Detailed: detail}
		if err != nil {
			s.Error = err.Error()
		}
		rep.Steps = append(rep.Steps, s)
		return err
	}

	roots, err := loadRoots(o.CAFile)
	if err != nil {
		return rep, err
	}
	token := o.Token
	if token == "" {
		if err := step("sts-token-exchange", func() (string, error) {
			t, err := exchange(ctx, o, roots)
			token = t
			return "", err
		}); err != nil {
			return rep, err
		}
	}

	conn, err := dial(o, roots)
	if err != nil {
		return rep, err
	}
	defer conn.Close()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	caps := repb.NewCapabilitiesClient(conn)
	cas := repb.NewContentAddressableStorageClient(conn)
	ac := repb.NewActionCacheClient(conn)

	if err := step("get-capabilities", func() (string, error) {
		c, err := caps.GetCapabilities(ctx, &repb.GetCapabilitiesRequest{InstanceName: o.InstanceName})
		if err != nil {
			return "", err
		}
		if c.GetCacheCapabilities() == nil {
			return "", errors.New("server reports no cache capabilities")
		}
		return fmt.Sprintf("digest functions %v", c.GetCacheCapabilities().GetDigestFunctions()), nil
	}); err != nil {
		return rep, err
	}

	blob := make([]byte, 96)
	if _, err := rand.Read(blob); err != nil {
		return rep, err
	}
	copy(blob, "cucina-canary ")
	blobDigest := digestOf(blob)
	if err := step("cas-write", func() (string, error) {
		r, err := cas.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{InstanceName: o.InstanceName,
			Requests: []*repb.BatchUpdateBlobsRequest_Request{{Digest: blobDigest, Data: blob}}})
		if err != nil {
			return "", err
		}
		for _, x := range r.GetResponses() {
			if x.GetStatus().GetCode() != 0 {
				return "", fmt.Errorf("BatchUpdateBlobs: %s", x.GetStatus().GetMessage())
			}
		}
		return blobDigest.GetHash(), nil
	}); err != nil {
		return rep, err
	}
	if err := step("cas-read", func() (string, error) {
		r, err := cas.BatchReadBlobs(ctx, &repb.BatchReadBlobsRequest{InstanceName: o.InstanceName, Digests: []*repb.Digest{blobDigest}})
		if err != nil {
			return "", err
		}
		rs := r.GetResponses()
		if len(rs) != 1 || rs[0].GetStatus().GetCode() != 0 {
			return "", fmt.Errorf("BatchReadBlobs: blob not returned (%v)", rs)
		}
		if !bytes.Equal(rs[0].GetData(), blob) {
			return "", errors.New("BatchReadBlobs returned different content")
		}
		return "", nil
	}); err != nil {
		return rep, err
	}

	actionBytes := make([]byte, 64)
	if _, err := rand.Read(actionBytes); err != nil {
		return rep, err
	}
	actionDigest := digestOf(actionBytes)
	want := &repb.ActionResult{
		ExitCode:    0,
		OutputFiles: []*repb.OutputFile{{Path: "canary.out", Digest: blobDigest}},
	}
	if err := step("ac-write", func() (string, error) {
		_, err := ac.UpdateActionResult(ctx, &repb.UpdateActionResultRequest{InstanceName: o.InstanceName, ActionDigest: actionDigest, ActionResult: want})
		return actionDigest.GetHash(), err
	}); err != nil {
		return rep, err
	}
	if err := step("ac-read", func() (string, error) {
		got, err := ac.GetActionResult(ctx, &repb.GetActionResultRequest{InstanceName: o.InstanceName, ActionDigest: actionDigest})
		if err != nil {
			return "", err
		}
		if len(got.GetOutputFiles()) != 1 || !proto.Equal(got.GetOutputFiles()[0].GetDigest(), blobDigest) {
			return "", errors.New("GetActionResult returned a different result")
		}
		return "", nil
	}); err != nil {
		return rep, err
	}
	rep.OK = true
	return rep, nil
}

func digestOf(b []byte) *repb.Digest {
	sum := sha256.Sum256(b)
	return &repb.Digest{Hash: hex.EncodeToString(sum[:]), SizeBytes: int64(len(b))}
}

func loadRoots(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA file %s: no PEM certificates", caFile)
	}
	return pool, nil
}

func dial(o Options, roots *x509.CertPool) (*grpc.ClientConn, error) {
	target := o.Endpoint
	opts := append([]grpc.DialOption{}, o.DialOptions...)
	switch {
	case strings.HasPrefix(target, "grpcs://"):
		target = strings.TrimPrefix(target, "grpcs://")
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: o.ServerName})))
	case strings.HasPrefix(target, "grpc://"):
		return nil, errors.New("canary: plaintext endpoints are not supported (UC22: every external endpoint uses TLS)")
	default:
		if len(o.DialOptions) == 0 {
			opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: o.ServerName})))
		}
	}
	return grpc.NewClient(target, opts...)
}

// exchange trades the service-account key for a Cucina JWT (RFC 8693).
func exchange(ctx context.Context, o Options, roots *x509.CertPool) (string, error) {
	key, err := os.ReadFile(o.KeyFile)
	if err != nil {
		return "", fmt.Errorf("service-account key: %w", err)
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}},
			// Never follow redirects with a credential in the body.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	form := url.Values{
		"grant_type":         {tokenExchangeGrant},
		"subject_token":      {strings.TrimSpace(string(key))},
		"subject_token_type": {serviceKeyTokenType},
		"audience":           {o.InstanceName},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(o.STSURL, "/")+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("STS: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		return "", fmt.Errorf("STS refused the exchange: HTTP %d %s %s", resp.StatusCode, e.Error, e.Description)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", errors.New("STS response carries no access_token")
	}
	return tr.AccessToken, nil
}
