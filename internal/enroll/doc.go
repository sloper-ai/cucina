// SPDX-License-Identifier: FSL-1.1-ALv2

// Package enroll implements the EnrollmentService (R-SEC-3): EC2 workers prove
// their identity with the AWS-signed instance identity document (RSA-2048
// PKCS#7, cross-checked with a tag-filtered Describe and the autoscaler's launch
// ledger, once per launch), Mac hosts with a multi-use site enrollment token
// plus serial-number admission; both receive short-lived certificates from
// internal/pki. The package also provides the certificate side of HostService
// (RenewCertificate, IssueVMIdentity) and the management hooks behind
// `cucinactl hosts …` (Admin). docs/security.md §Enrollment is the reference.
//
// The server runs on every controller replica on the enrollment listener with
// TLS server authentication only (callers have no certificate yet); all shared
// state lives in the stores, which are Kubernetes objects in production:
// MacHostStore (MacHost CR), SecretTokens (one Secret) and LeaseReplay (one
// Lease per enrolled EC2 instance). The Memory* stores serve tests.
//
// Wiring in cucina-controller (sketch):
//
//	identity, _ := enroll.NewIdentityVerifier()
//	srv, err := enroll.New(enroll.Deps{
//		Issuer: issuer, Clock: clock, Logger: log, Pools: poolSet, // pools.Set implements PoolSettingsProvider
//		// EC2 only (all four or none):
//		Identity: identity, Compute: compute,
//		Launches: enroll.LedgerLaunches{Cluster: cfg.ClusterID, Ledger: func(ctx context.Context, p domain.PoolName) (scaling.Ledger, error) {
//			var wp v1alpha1.WorkerPool
//			err := client.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: string(p)}, &wp)
//			return reconcile.ReadLedger(&wp), err
//		}},
//		Replay: &enroll.LeaseReplay{Reader: apiReader, Client: client, Namespace: cfg.Namespace},
//		Hosts:  &enroll.MacHostStore{Reader: apiReader, Client: client, Namespace: cfg.Namespace},
//		Tokens: &enroll.SecretTokens{Reader: apiReader, Client: client, Namespace: cfg.Namespace, Name: cfg.ReleaseName + "-enroll-tokens"},
//		Revoker: revoker, // adapts internal/keys (deny-list `sub` entries); optional
//		Options: enroll.Options{
//			ClusterID: cfg.ClusterID, AWSAccountID: cfg.AWS.AccountID, AWSRegion: cfg.AWS.Region,
//			HostEndpoint: cfg.Endpoints.HostEndpoint, DefaultTokenTTL: cfg.Hosts.DefaultTokenTTL.Duration,
//		},
//	})
//	srv.Register(enrollmentGRPCServer) // TLS, no client certificate
//	go srv.Run(ctx)                    // prunes launch Leases (every replica)
//	mgmtAdapter := srv.Admin()         // internal/mgmt.EnrollAdmin
package enroll
