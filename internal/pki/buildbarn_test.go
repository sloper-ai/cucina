// SPDX-License-Identifier: FSL-1.1-ALv2

package pki_test

import (
	"context"
	"crypto/x509"
	"net"
	"testing"
	"time"

	bbauth "github.com/buildbarn/bb-storage/pkg/auth"
	bbclock "github.com/buildbarn/bb-storage/pkg/clock"
	"github.com/buildbarn/bb-storage/pkg/digest"
	bbjmespath "github.com/buildbarn/bb-storage/pkg/jmespath"
	bbx509 "github.com/buildbarn/bb-storage/pkg/x509"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

// bbClock adapts a fixed time to Buildbarn's clock interface.
type bbClock struct{ now time.Time }

func (c bbClock) Now() time.Time { return c.now }
func (bbClock) NewContextWithTimeout(p context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(p, d)
}
func (bbClock) NewTimer(d time.Duration) (bbclock.Timer, <-chan time.Time) {
	t := time.NewTimer(d)
	return t, t.C
}
func (bbClock) NewTicker(d time.Duration) (bbclock.Ticker, <-chan time.Time) {
	t := time.NewTicker(d)
	return t, t.C
}

// TestBuildbarnAcceptsOnlyDocumentedIdentities runs certificates issued by the
// real profiles through Buildbarn's own tlsClientCertificate verifier and
// JMESPath authorizers (bb-storage at the pinned version; the verifier code is
// identical in both pinned bb-storage tags) with the expressions the chart
// renders. Guards R-SEC-2 (worker listener: workers and hosts; scheduler worker
// API: workers; AC writes only for workload identities on that listener),
// R-SEC-4 (BuildQueueState: controller only) and T10(h) at the unit tier: a
// worker or host without a valid certificate is rejected.
func TestBuildbarnAcceptsOnlyDocumentedIdentities(t *testing.T) {
	clock := pkitest.NewClock(pkitest.Epoch)
	iss, ca := pkitest.NewIssuer(t, clock)
	foreignIss, _ := pkitest.NewIssuer(t, clock)
	key := pkitest.Key(t)

	chain := func(i *pki.Issued, err error) []*x509.Certificate {
		require.NoError(t, err)
		return pkitest.ParseChain(t, i.ChainPEM)
	}
	certs := map[string][]*x509.Certificate{
		"worker":            chain(iss.IssueWorker(pkitest.CSR(t, key, nil), "linux-x86-64", "i-0123456789abcdef0")),
		"vm":                chain(iss.IssueVM(pkitest.CSR(t, key, nil), "macos-arm64", "H4X9K2LM7Q", "vm-1")),
		"host":              chain(iss.IssueHost(pkitest.CSR(t, key, nil), "H4X9K2LM7Q")),
		"controller":        chain(iss.IssueController(key.Public())),
		"server-clientauth": chain(iss.IssueServer(key.Public(), "frontend", []string{"frontend"}, []net.IP{net.ParseIP("127.0.0.1")}, true, 0)),
		"server":            chain(iss.IssueServer(key.Public(), "scheduler", []string{"scheduler"}, nil, false, 0)),
		"foreign-worker":    chain(foreignIss.IssueWorker(pkitest.CSR(t, key, nil), "linux-x86-64", "i-0123456789abcdef0")),
		"none":              nil,
	}

	type listener struct {
		validation, metadata string
	}
	listeners := map[string]listener{
		"frontend-workers":  {pki.BuildbarnWorkerListenerValidation, pki.BuildbarnWorkerListenerMetadata([]string{"main"})},
		"scheduler-workers": {pki.BuildbarnSchedulerWorkerValidation, pki.BuildbarnSubjectMetadata},
		"buildqueuestate":   {pki.BuildbarnBuildQueueStateValidation, pki.BuildbarnSubjectMetadata},
	}
	accept := map[string]map[string]bool{
		"frontend-workers":  {"worker": true, "vm": true, "host": true},
		"scheduler-workers": {"worker": true, "vm": true},
		"buildqueuestate":   {"controller": true},
	}
	authenticate := func(l listener, at time.Time, chain []*x509.Certificate) (*bbauth.AuthenticationMetadata, error) {
		v := bbx509.NewClientCertificateVerifier(ca.Pool(), bbClock{at}, bbjmespath.MustCompile(l.validation), bbjmespath.MustCompile(l.metadata))
		return v.VerifyClientCertificate(chain)
	}

	for lname, l := range listeners {
		for cname, chain := range certs {
			_, err := authenticate(l, clock.Now(), chain)
			if accept[lname][cname] {
				require.NoError(t, err, "%s must accept %s", lname, cname)
			} else {
				require.Error(t, err, "%s must reject %s", lname, cname)
			}
		}
	}
	// An expired worker or host certificate is rejected everywhere.
	for _, cname := range []string{"worker", "host"} {
		_, err := authenticate(listeners["frontend-workers"], certs[cname][0].NotAfter.Add(time.Second), certs[cname])
		require.Error(t, err, "expired %s", cname)
	}

	// Authorizers over the extracted metadata.
	authorize := func(expr string, md *bbauth.AuthenticationMetadata, instance string) bool {
		ctx := bbauth.NewContextWithAuthenticationMetadata(context.Background(), md)
		name, err := digest.NewInstanceName(instance)
		require.NoError(t, err)
		errs := bbauth.NewJMESPathExpressionAuthorizer(bbjmespath.MustCompile(expr)).Authorize(ctx, []digest.InstanceName{name})
		return errs[0] == nil
	}
	mdOf := func(lname, cname string) *bbauth.AuthenticationMetadata {
		md, err := authenticate(listeners[lname], clock.Now(), certs[cname])
		require.NoError(t, err)
		return md
	}
	const acPut = "contains(authenticationMetadata.private.ac_write, instanceName)"
	require.True(t, authorize(acPut, mdOf("frontend-workers", "worker"), "main"), "workers write the AC of their instance names")
	require.True(t, authorize(acPut, mdOf("frontend-workers", "host"), "main"), "a host's L2 forwards its VMs' AC writes")
	require.False(t, authorize(acPut, mdOf("frontend-workers", "worker"), "tenant-b"), "only listed instance names")
	require.True(t, authorize(pki.BuildbarnSynchronizeAuthorizer, mdOf("scheduler-workers", "vm"), "main"))
	require.False(t, authorize(pki.BuildbarnBuildQueueStateAuthorizer, mdOf("scheduler-workers", "worker"), "main"))
	require.True(t, authorize(pki.BuildbarnBuildQueueStateAuthorizer, mdOf("buildqueuestate", "controller"), "main"))
	require.Equal(t, "spiffe://cucina/host/H4X9K2LM7Q", mdOf("frontend-workers", "host").GetRaw()["private"].(map[string]any)["sub"],
		"private.sub is the URI SAN the deny-list matches")
}
