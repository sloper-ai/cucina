// SPDX-License-Identifier: FSL-1.1-ALv2

package bbtest

import (
	"encoding/json"
	"fmt"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_scheduler"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/scheduler"
	authpb "github.com/buildbarn/bb-storage/pkg/proto/auth"
	authconf "github.com/buildbarn/bb-storage/pkg/proto/configuration/auth"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/blobstore"
	grpcpb "github.com/buildbarn/bb-storage/pkg/proto/configuration/grpc"
	jmespathpb "github.com/buildbarn/bb-storage/pkg/proto/configuration/jmespath"
	tlspb "github.com/buildbarn/bb-storage/pkg/proto/configuration/tls"
	x509pb "github.com/buildbarn/bb-storage/pkg/proto/configuration/x509"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Workload identity rules of docs/security.md, as the chart renders them.
const (
	WorkerValidation        = "length(uris) == `1` && starts_with(uris[0], 'spiffe://cucina/worker/')"
	WorkerOrHostValidation  = "length(uris) == `1` && (starts_with(uris[0], 'spiffe://cucina/worker/') || starts_with(uris[0], 'spiffe://cucina/host/'))"
	ControllerValidation    = "length(uris) == `1` && uris[0] == 'spiffe://cucina/controller'"
	CertificateMetadata     = "{public: {user: uris[0]}, private: {sub: uris[0]}}"
	ControllerURI           = "spiffe://cucina/controller"
	requestMetadataForward  = `{"build.bazel.remote.execution.v2.requestmetadata-bin": incomingGRPCMetadata."build.bazel.remote.execution.v2.requestmetadata-bin"}`
	defaultMaximumMessageSz = 16 << 20
)

// StorageOptions configure StorageConfig: one bb_storage acting as frontend and
// storage, with in-memory stores (a test fixture, not the chart's profiles).
type StorageOptions struct {
	// ClientListen is a plaintext listener without authentication (test
	// clients, the scheduler's CAS client).
	ClientListen string
	// WorkerListen, if set, is an mTLS listener admitting worker and host
	// identities (like the frontend's worker listener) with ServerKeyPair.
	WorkerListen  string
	ServerKeyPair *KeyPair
	CAPEM         string
	// SchedulerAddress, if set, forwards Execute for every instance name there
	// (plaintext), with the REAPI request metadata (R-RE-5).
	SchedulerAddress string
	// Compression advertises ZSTD and enables a bounded zstd pool (R-DATA-3).
	Compression bool
	// CASBlockBytes is the in-memory CAS block size (largest blob); default 16 MiB.
	CASBlockBytes           int64
	MaximumMessageSizeBytes int64
}

// StorageConfig renders a bb_storage configuration in the NEW nested
// keyLocationMap schema of the pinned bb_storage (ADR 0001).
func StorageConfig(o StorageOptions) []byte {
	if o.CASBlockBytes == 0 {
		o.CASBlockBytes = 16 << 20
	}
	if o.MaximumMessageSizeBytes == 0 {
		o.MaximumMessageSizeBytes = defaultMaximumMessageSz
	}
	allow := map[string]any{"allow": map[string]any{}}
	local := func(blockBytes int64, entries int) map[string]any {
		return map[string]any{"local": map[string]any{
			"keyLocationMap": map[string]any{
				"inMemory":           map[string]any{"entries": entries},
				"maximumGetAttempts": 16,
				"maximumPutAttempts": 64,
			},
			"oldBlocks":      1,
			"currentBlocks":  1,
			"newBlocks":      1,
			"blocksInMemory": map[string]any{"blockSizeBytes": blockBytes},
		}}
	}
	servers := []any{map[string]any{
		"listenAddresses":                 []string{o.ClientListen},
		"authenticationPolicy":            allow,
		"maximumReceivedMessageSizeBytes": o.MaximumMessageSizeBytes,
	}}
	if o.WorkerListen != "" {
		servers = append(servers, map[string]any{
			"listenAddresses": []string{o.WorkerListen},
			"authenticationPolicy": map[string]any{"tlsClientCertificate": map[string]any{
				"clientCertificateAuthorities":         o.CAPEM,
				"validationJmespathExpression":         map[string]any{"expression": WorkerOrHostValidation},
				"metadataExtractionJmespathExpression": map[string]any{"expression": CertificateMetadata},
			}},
			"tls": map[string]any{"serverKeyPair": map[string]any{"files": map[string]any{
				"certificatePath": o.ServerKeyPair.CertPath,
				"privateKeyPath":  o.ServerKeyPair.KeyPath,
				"refreshInterval": "300s",
			}}},
			"maximumReceivedMessageSizeBytes": o.MaximumMessageSizeBytes,
			// As the chart's frontend (R-CP-5): host L2s ping every 60 s.
			"keepaliveEnforcementPolicy": map[string]any{"minTime": "20s", "permitWithoutStream": true},
		})
	}
	cfg := map[string]any{
		"grpcServers": servers,
		"contentAddressableStorage": map[string]any{
			"backend":               local(o.CASBlockBytes, 1<<16),
			"getAuthorizer":         allow,
			"putAuthorizer":         allow,
			"findMissingAuthorizer": allow,
		},
		"actionCache": map[string]any{
			"backend": map[string]any{"completenessChecking": map[string]any{
				"backend":                   local(1<<20, 1<<14),
				"maximumTotalTreeSizeBytes": 64 << 20,
			}},
			"getAuthorizer": allow,
			"putAuthorizer": allow,
		},
		"fileSystemAccessCache": map[string]any{
			"backend":       local(1<<20, 1<<14),
			"getAuthorizer": allow,
			"putAuthorizer": allow,
		},
		"maximumMessageSizeBytes": o.MaximumMessageSizeBytes,
	}
	if o.SchedulerAddress != "" {
		cfg["schedulers"] = map[string]any{"": map[string]any{"endpoint": map[string]any{
			"address":                       o.SchedulerAddress,
			"addMetadataJmespathExpression": map[string]any{"expression": requestMetadataForward},
		}}}
		cfg["executeAuthorizer"] = allow
	}
	if o.Compression {
		cfg["supportedCompressors"] = []string{"ZSTD"}
		cfg["zstdPool"] = map[string]any{"maximumEncoders": 8, "maximumDecoders": 8}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		panic(err)
	}
	return b
}

// Queue is one predeclared platform queue.
type Queue struct {
	InstanceNamePrefix string
	// Properties are name/value pairs; SchedulerConfig sorts them.
	Properties  map[string]string
	SizeClasses []uint32
}

// SchedulerOptions configure SchedulerConfig.
type SchedulerOptions struct {
	// ClientListen accepts Execute without authentication (the frontend's
	// forwarding target and test clients).
	ClientListen string
	// WorkerListen serves Synchronize; with ServerKeyPair it requires worker
	// certificates (spiffe://cucina/worker/…) like the chart's scheduler.
	WorkerListen string
	// BuildQueueStateListen serves BuildQueueState; with ServerKeyPair it
	// requires the controller certificate (spiffe://cucina/controller).
	BuildQueueStateListen string
	ServerKeyPair         *KeyPair
	CAPEM                 string
	// StorageAddress is the plaintext CAS endpoint (bb_storage ClientListen).
	StorageAddress          string
	Queues                  []Queue
	MaximumMessageSizeBytes int64
}

// SchedulerConfig renders a bb_scheduler configuration, typed against the
// pinned bb-remote-execution protos (OLD bb-storage schema).
func SchedulerConfig(o SchedulerOptions) []byte {
	if o.MaximumMessageSizeBytes == 0 {
		o.MaximumMessageSizeBytes = defaultMaximumMessageSz
	}
	allowPolicy := &grpcpb.AuthenticationPolicy{Policy: &grpcpb.AuthenticationPolicy_Allow{Allow: &authpb.AuthenticationMetadata{}}}
	allow := &authconf.AuthorizerConfiguration{Policy: &authconf.AuthorizerConfiguration_Allow{Allow: &emptypb.Empty{}}}
	server := func(addr, validation string) *grpcpb.ServerConfiguration {
		s := &grpcpb.ServerConfiguration{
			ListenAddresses:                 []string{addr},
			AuthenticationPolicy:            allowPolicy,
			MaximumReceivedMessageSizeBytes: o.MaximumMessageSizeBytes,
		}
		if o.ServerKeyPair != nil {
			s.AuthenticationPolicy = &grpcpb.AuthenticationPolicy{Policy: &grpcpb.AuthenticationPolicy_TlsClientCertificate{
				TlsClientCertificate: &x509pb.ClientCertificateVerifierConfiguration{
					ClientCertificateAuthorities:         o.CAPEM,
					ValidationJmespathExpression:         &jmespathpb.Expression{Expression: validation},
					MetadataExtractionJmespathExpression: &jmespathpb.Expression{Expression: CertificateMetadata},
				},
			}}
			s.TransportSecurity = &grpcpb.ServerConfiguration_Tls{Tls: &tlspb.ServerConfiguration{
				ServerKeyPair: &tlspb.X509KeyPair{KeyPair: &tlspb.X509KeyPair_Files_{Files: &tlspb.X509KeyPair_Files{
					CertificatePath: o.ServerKeyPair.CertPath,
					PrivateKeyPath:  o.ServerKeyPair.KeyPath,
					RefreshInterval: durationpb.New(5 * time.Minute),
				}}},
			}}
		}
		return s
	}
	jmes := func(expr string) *authconf.AuthorizerConfiguration {
		if o.ServerKeyPair == nil {
			return allow
		}
		return &authconf.AuthorizerConfiguration{Policy: &authconf.AuthorizerConfiguration_JmespathExpression{
			JmespathExpression: &jmespathpb.Expression{Expression: expr},
		}}
	}
	cfg := &bb_scheduler.ApplicationConfiguration{
		ClientGrpcServers: []*grpcpb.ServerConfiguration{{
			ListenAddresses:                 []string{o.ClientListen},
			AuthenticationPolicy:            allowPolicy,
			MaximumReceivedMessageSizeBytes: o.MaximumMessageSizeBytes,
		}},
		WorkerGrpcServers:          []*grpcpb.ServerConfiguration{server(o.WorkerListen, WorkerValidation)},
		BuildQueueStateGrpcServers: []*grpcpb.ServerConfiguration{server(o.BuildQueueStateListen, ControllerValidation)},
		ContentAddressableStorage: &blobstore.BlobAccessConfiguration{Backend: &blobstore.BlobAccessConfiguration_Grpc{
			Grpc: &blobstore.GrpcBlobAccessConfiguration{Client: &grpcpb.ClientConfiguration{Address: o.StorageAddress}},
		}},
		MaximumMessageSizeBytes:           o.MaximumMessageSizeBytes,
		ExecuteAuthorizer:                 allow,
		ModifyDrainsAuthorizer:            jmes("authenticationMetadata.private.sub == '" + ControllerURI + "'"),
		KillOperationsAuthorizer:          jmes("authenticationMetadata.private.sub == '" + ControllerURI + "'"),
		SynchronizeAuthorizer:             jmes("starts_with(authenticationMetadata.private.sub, 'spiffe://cucina/worker/')"),
		PlatformQueueWithNoWorkersTimeout: durationpb.New(900 * time.Second),
		ActionRouter: &scheduler.ActionRouterConfiguration{Kind: &scheduler.ActionRouterConfiguration_Simple{
			Simple: &scheduler.SimpleActionRouterConfiguration{
				PlatformKeyExtractor: &scheduler.PlatformKeyExtractorConfiguration{Kind: &scheduler.PlatformKeyExtractorConfiguration_Action{Action: &emptypb.Empty{}}},
				InvocationKeyExtractors: []*scheduler.InvocationKeyExtractorConfiguration{
					{Kind: &scheduler.InvocationKeyExtractorConfiguration_CorrelatedInvocationsId{CorrelatedInvocationsId: &emptypb.Empty{}}},
					{Kind: &scheduler.InvocationKeyExtractorConfiguration_ToolInvocationId{ToolInvocationId: &emptypb.Empty{}}},
				},
				InitialSizeClassAnalyzer: &scheduler.InitialSizeClassAnalyzerConfiguration{
					DefaultExecutionTimeout: durationpb.New(30 * time.Minute),
					MaximumExecutionTimeout: durationpb.New(time.Hour),
				},
			},
		}},
	}
	for _, q := range o.Queues {
		cfg.PredeclaredPlatformQueues = append(cfg.PredeclaredPlatformQueues, &bb_scheduler.PredeclaredPlatformQueueConfiguration{
			InstanceNamePrefix: q.InstanceNamePrefix,
			Platform:           Platform(q.Properties),
			SizeClasses:        q.SizeClasses,
		})
	}
	b, err := protojson.MarshalOptions{Multiline: true}.Marshal(cfg)
	if err != nil {
		panic(fmt.Sprintf("scheduler config: %v", err))
	}
	return b
}

// Platform returns properties as a REAPI Platform sorted by name, then value
// (the order Buildbarn requires).
func Platform(props map[string]string) *remoteexecution.Platform {
	p := &remoteexecution.Platform{}
	for _, k := range sortedKeys(props) {
		p.Properties = append(p.Properties, &remoteexecution.Platform_Property{Name: k, Value: props[k]})
	}
	return p
}
