// SPDX-License-Identifier: FSL-1.1-ALv2

// Package grpcuploads sanitizes Bazel 9.2 remote_grpc_log streams and joins
// client-offered CAS payload to explicit repository/version manifests (X5).
// Raw logs can contain request/action data and MUST remain on private storage.
package grpcuploads

import "time"

// Source records the pinned wire/metadata contract, not a guarantee that a
// whole raw RPC log is secret-free. LoggingInterceptor extracts RequestMetadata
// only; WriteHandler retains counts, not WriteRequest.data. Other handlers
// retain ActionResults and may therefore contain arbitrary build output.
const Source = "bazelbuild/bazel 9.2.0: remote_execution_log.proto; remote/logging/LoggingInterceptor.java; WriteHandler.java; RemoteRepoContentsCacheImpl.buildContext"

// Repository is an explicitly resolved component release and artifact variant.
// Canonical names alone are not versions (Bzlmod commonly omits versions).
type Repository struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	Version   string `json:"version"`
	Variant   string `json:"variant"`
	Pin       string `json:"pin"` // immutable source integrity/commit or recorded rule key
}

type Inventory struct {
	SchemaVersion int          `json:"schemaVersion"`
	Repositories  []Repository `json:"repositories"`
}

// Digest identifies uncompressed content, regardless of compression or path.
type Digest struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

func (d Digest) Key() string { return d.Hash + "/" + itoa(d.Size) }

// Options binds a raw log to a known invocation/CAS. Scope is an opaque hash
// of endpoint+instance, not a hostname/IP that belongs in a public report.
type Options struct {
	Config       string
	InvocationID string
	Scope        string
	Instance     string
	HashFunction string
	Inventory    Inventory
}

// Manifest contains all digests sent to FindMissingBlobs while uploading a
// reproducible repository, plus known housekeeping digests from the AC record.
type Manifest struct {
	Repository   string   `json:"repository"`
	Digests      []Digest `json:"digests"`
	Housekeeping []Digest `json:"housekeeping,omitempty"`
	Tree         Digest   `json:"tree"`
	Complete     bool     `json:"complete"` // successful full FMB phase followed by repo AC update
}

// Write is allow-listed evidence only. OfferedBytes is WriteHandler.bytes_sent:
// bytes offered to gRPC, compressed when zstd, not socket bytes or newly stored
// bytes. It includes failed/retried/already-present attempts.
type Write struct {
	Repository    string    `json:"repository,omitempty"`
	Digest        Digest    `json:"digest"`
	Upload        string    `json:"upload"`
	Compressor    string    `json:"compressor"`
	Status        int32     `json:"status"`
	OfferedBytes  int64     `json:"offeredBytes"`
	Offsets       []int64   `json:"offsets,omitempty"`
	FinishWrites  []int64   `json:"finishWrites,omitempty"`
	CommittedSize *int64    `json:"committedSize,omitempty"`
	Started       time.Time `json:"started"`
	Finished      time.Time `json:"finished"`
}

// Query is a sanitized resume/completion confirmation for one upload UUID.
type Query struct {
	Digest        Digest `json:"digest"`
	Upload        string `json:"upload"`
	Compressor    string `json:"compressor"`
	CommittedSize int64  `json:"committedSize"`
	Complete      bool   `json:"complete"`
	Status        int32  `json:"status"`
}

// Trace deliberately has no generic headers, commands, status messages,
// request/response payloads, stdout/stderr, or arbitrary metadata fields.
type Trace struct {
	SchemaVersion    int        `json:"schemaVersion"`
	InventoryDigest  string     `json:"inventoryDigest"`  // binds the explicit version/variant/source pins
	ClientProvenance string     `json:"clientProvenance"` // hash of actual-client resolved repository definitions
	Config           string     `json:"config"`
	InvocationID     string     `json:"invocationId"`
	Scope            string     `json:"scope"`
	HashFunction     string     `json:"hashFunction"`
	Entries          int        `json:"entries"` // RPCs carrying the expected invocation identity
	Manifests        []Manifest `json:"manifests"`
	Writes           []Write    `json:"writes"`
	Queries          []Query    `json:"queries,omitempty"`
	Issues           []string   `json:"issues,omitempty"`
}

// File is supplementary compact execution-log evidence; paths are deliberately
// omitted from the exported ledger. Membership covers used files, not the full
// repository. An execution log is not a replacement for a cold FMB manifest.
type File struct {
	Repository string `json:"repository"`
	Digest     Digest `json:"digest"`
}

type Capture struct {
	Trace Trace  `json:"trace"`
	Files []File `json:"files,omitempty"`
}

type Component struct {
	Repository         Repository `json:"repository"`
	UniqueContentBytes int64      `json:"uniqueContentBytes"`
	OfferedBytes       int64      `json:"offeredBytes"`
	HousekeepingBytes  int64      `json:"housekeepingBytes"`
	Configurations     []string   `json:"configurations"`
}

type Report struct {
	Pass                         bool        `json:"pass"`
	Components                   []Component `json:"components"`
	UniqueContentBytes           int64       `json:"uniqueContentBytes"` // global de-duplication, including shared blobs once
	OfferedBytes                 int64       `json:"offeredBytes"`
	RetryAttempts                int         `json:"retryAttempts"`
	RepeatedConfigurationPayload int64       `json:"repeatedConfigurationPayload"`
	Violations                   []string    `json:"violations,omitempty"`
	Unavailable                  []string    `json:"unavailable,omitempty"`
}
