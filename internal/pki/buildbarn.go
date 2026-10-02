// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"encoding/json"
	"strings"
)

// Buildbarn tlsClientCertificate expressions (bb-storage
// ClientCertificateVerifierConfiguration, evaluated by go-jmespath v0.4.0 over
// {"dnsNames": […], "emailAddresses": […], "uris": […]}). The chart renders these
// verbatim; docs/security.md §Workload identity is the normative copy and the
// tests in this package keep both honest against certificates issued by the
// real profiles.
const (
	// BuildbarnWorkerListenerValidation admits worker (EC2 and VM) and host
	// identities on the frontend's worker/host listener.
	BuildbarnWorkerListenerValidation = "length(uris) == `1` && (starts_with(uris[0], '" + WorkerURIPrefix +
		"') || starts_with(uris[0], '" + HostURIPrefix + "'))"

	// BuildbarnSchedulerWorkerValidation admits only worker identities on the
	// scheduler's workerGrpcServers (hosts never synchronize).
	BuildbarnSchedulerWorkerValidation = "length(uris) == `1` && starts_with(uris[0], '" + WorkerURIPrefix + "')"

	// BuildbarnBuildQueueStateValidation admits only the controller on the
	// scheduler's buildQueueStateGrpcServers (R-SEC-4).
	BuildbarnBuildQueueStateValidation = "length(uris) == `1` && uris[0] == '" + ControllerURI + "'"

	// BuildbarnStorageFromFrontendValidation optionally restricts storage shards
	// to the frontend's client certificate.
	BuildbarnStorageFromFrontendValidation = "length(uris) == `1` && uris[0] == '" + ServerURIPrefix + "frontend'"

	// BuildbarnSubjectMetadata is the metadata extraction for listeners whose
	// authorizers only need the subject (scheduler worker and BuildQueueState
	// listeners).
	BuildbarnSubjectMetadata = "{public: {user: uris[0]}, private: {sub: uris[0]}}"

	// BuildbarnSynchronizeAuthorizer is the scheduler's synchronizeAuthorizer
	// (AND the deny-list fragment of docs/security.md §Deny-list).
	BuildbarnSynchronizeAuthorizer = "starts_with(authenticationMetadata.private.sub, '" + WorkerURIPrefix + "')"

	// BuildbarnBuildQueueStateAuthorizer is the scheduler's modifyDrainsAuthorizer
	// and killOperationsAuthorizer.
	BuildbarnBuildQueueStateAuthorizer = "authenticationMetadata.private.sub == '" + ControllerURI + "'"
)

// BuildbarnWorkerListenerMetadata returns the metadataExtractionJmespathExpression
// of the frontend's worker/host listener: certificate holders get the same
// private keys as a Cucina JWT's `cucina` claim object (explicit instance-name
// lists per verb) plus `sub` = the URI SAN, so one set of authorizers serves
// JWT clients and certificates (docs/security.md §Workload identity, rule 3).
func BuildbarnWorkerListenerMetadata(instanceNames []string) string {
	list := jmespathLiteral(instanceNames)
	return "{public: {user: uris[0]}, private: {sub: uris[0], cas_read: " + list + ", cas_write: " + list +
		", ac_read: " + list + ", ac_write: " + list + ", execute: `[]`}}"
}

// jmespathLiteral renders v as a JMESPath JSON literal (`…`).
func jmespathLiteral(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	if err != nil { // unreachable for []string
		panic(err)
	}
	return "`" + strings.ReplaceAll(string(b), "`", "\\`") + "`"
}
