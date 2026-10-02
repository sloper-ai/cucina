// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// TrustDomain is the SPIFFE trust domain of every Cucina workload identity.
// One CA serves exactly one Cucina installation, so the trust domain is a
// constant (docs/security.md §Workload identity).
const TrustDomain = "cucina"

// Exact URI SAN strings and prefixes. Buildbarn's tlsClientCertificate
// expressions (buildbarn.go) match these byte for byte.
const (
	WorkerURIPrefix = "spiffe://cucina/worker/" // EC2 workers and macOS VM workers
	HostURIPrefix   = "spiffe://cucina/host/"
	ServerURIPrefix = "spiffe://cucina/server/"
	ControllerURI   = "spiffe://cucina/controller"
)

// Role is the kind of a workload identity.
type Role string

// Roles. RoleWorker and RoleVM share the URI prefix WorkerURIPrefix ("any
// worker"); they differ in the number of path segments.
const (
	RoleWorker     Role = "worker"     // spiffe://cucina/worker/<pool>/<instance-id>
	RoleVM         Role = "vm"         // spiffe://cucina/worker/<pool>/<serial>/<vm>
	RoleHost       Role = "host"       // spiffe://cucina/host/<serial>
	RoleController Role = "controller" // spiffe://cucina/controller
	RoleServer     Role = "server"     // spiffe://cucina/server/<component> (+ DNS/IP SANs)
)

// ErrInvalidIdentity is returned for identities outside the grammar of
// docs/security.md §Workload identity.
var ErrInvalidIdentity = errors.New("invalid workload identity")

// Segment grammars (docs/security.md §Workload identity, "Segment grammar").
var (
	poolRE      = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)
	instanceRE  = regexp.MustCompile(`^i-([0-9a-f]{8}|[0-9a-f]{17})$`)
	serialRE    = regexp.MustCompile(`^[A-Z0-9]{6,32}$`)
	vmRE        = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	componentRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Identity is a parsed Cucina workload identity. Only the fields of its Role
// are set.
type Identity struct {
	Role       Role
	Pool       string // RoleWorker, RoleVM
	InstanceID string // RoleWorker
	Serial     string // RoleHost, RoleVM (canonical upper case)
	VM         string // RoleVM
	Component  string // RoleServer
}

// WorkerIdentity is the identity of an EC2 worker.
func WorkerIdentity(pool, instanceID string) (Identity, error) {
	id := Identity{Role: RoleWorker, Pool: pool, InstanceID: instanceID}
	return id, id.Validate()
}

// VMIdentity is the identity of a macOS VM worker on the host with the given
// serial number. The serial must already be canonical (see CanonicalSerial).
func VMIdentity(pool, serial, vm string) (Identity, error) {
	id := Identity{Role: RoleVM, Pool: pool, Serial: serial, VM: vm}
	return id, id.Validate()
}

// HostIdentity is the identity of a Mac host. The serial must already be
// canonical (see CanonicalSerial).
func HostIdentity(serial string) (Identity, error) {
	id := Identity{Role: RoleHost, Serial: serial}
	return id, id.Validate()
}

// ControllerIdentity is the identity of the controller's BuildQueueState client.
func ControllerIdentity() Identity { return Identity{Role: RoleController} }

// ServerIdentity is the URI SAN identity of an in-cluster server component.
func ServerIdentity(component string) (Identity, error) {
	id := Identity{Role: RoleServer, Component: component}
	return id, id.Validate()
}

// CanonicalSerial returns the canonical form of a Mac serial number: ASCII
// upper case without surrounding white space, matching [A-Z0-9]{6,32}.
func CanonicalSerial(s string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(s))
	if !serialRE.MatchString(c) {
		return "", fmt.Errorf("%w: serial number %q must be 6-32 ASCII letters or digits", ErrInvalidIdentity, s)
	}
	return c, nil
}

// Validate checks the identity against the grammar of its role.
func (id Identity) Validate() error {
	check := func(what, v string, re *regexp.Regexp) error {
		if !re.MatchString(v) {
			return fmt.Errorf("%w: %s %s %q does not match %s", ErrInvalidIdentity, id.Role, what, v, re.String())
		}
		return nil
	}
	var errs []error
	switch id.Role {
	case RoleWorker:
		errs = append(errs, check("pool", id.Pool, poolRE), check("instance id", id.InstanceID, instanceRE))
		if id.Serial != "" || id.VM != "" || id.Component != "" {
			errs = append(errs, fmt.Errorf("%w: worker identity with host/vm/component fields", ErrInvalidIdentity))
		}
	case RoleVM:
		errs = append(errs, check("pool", id.Pool, poolRE), check("serial", id.Serial, serialRE), check("vm", id.VM, vmRE))
		if id.InstanceID != "" || id.Component != "" {
			errs = append(errs, fmt.Errorf("%w: vm identity with instance/component fields", ErrInvalidIdentity))
		}
	case RoleHost:
		errs = append(errs, check("serial", id.Serial, serialRE))
		if id.Pool != "" || id.InstanceID != "" || id.VM != "" || id.Component != "" {
			errs = append(errs, fmt.Errorf("%w: host identity with extra fields", ErrInvalidIdentity))
		}
	case RoleController:
		if id != (Identity{Role: RoleController}) {
			errs = append(errs, fmt.Errorf("%w: controller identity with extra fields", ErrInvalidIdentity))
		}
	case RoleServer:
		errs = append(errs, check("component", id.Component, componentRE))
		if id.Pool != "" || id.InstanceID != "" || id.Serial != "" || id.VM != "" {
			errs = append(errs, fmt.Errorf("%w: server identity with extra fields", ErrInvalidIdentity))
		}
	default:
		errs = append(errs, fmt.Errorf("%w: unknown role %q", ErrInvalidIdentity, id.Role))
	}
	return errors.Join(errs...)
}

// String returns the URI SAN string. It does not validate; call Validate first
// for identities built from untrusted input.
func (id Identity) String() string {
	switch id.Role {
	case RoleWorker:
		return WorkerURIPrefix + id.Pool + "/" + id.InstanceID
	case RoleVM:
		return WorkerURIPrefix + id.Pool + "/" + id.Serial + "/" + id.VM
	case RoleHost:
		return HostURIPrefix + id.Serial
	case RoleController:
		return ControllerURI
	case RoleServer:
		return ServerURIPrefix + id.Component
	}
	return ""
}

// URL returns the URI SAN as a *url.URL suitable for x509.Certificate.URIs.
func (id Identity) URL() *url.URL {
	u, err := url.Parse(id.String())
	if err != nil {
		// Unreachable for valid identities: every segment is [A-Za-z0-9._-].
		panic(fmt.Sprintf("pki: identity %q does not parse as a URL: %v", id.String(), err))
	}
	return u
}

// IsWorker reports whether the identity may act as a Buildbarn worker (EC2 or VM).
func (id Identity) IsWorker() bool { return id.Role == RoleWorker || id.Role == RoleVM }

// Node returns the Buildbarn `node` worker-id label value of a worker identity:
// the EC2 instance ID, or "<serial>/<vm>" for a macOS VM. It is empty for other roles.
func (id Identity) Node() string {
	switch id.Role {
	case RoleWorker:
		return id.InstanceID
	case RoleVM:
		return id.Serial + "/" + id.VM
	}
	return ""
}

// ParseIdentity parses a URI SAN string. Only the exact canonical form is
// accepted: no percent-encoding, user info, port, query, fragment, empty, "."
// or ".." segments, and the segment count and grammar of the role.
func ParseIdentity(s string) (Identity, error) {
	const scheme = "spiffe://" + TrustDomain + "/"
	if !strings.HasPrefix(s, scheme) {
		return Identity{}, fmt.Errorf("%w: %q is not a spiffe://%s/ URI", ErrInvalidIdentity, s, TrustDomain)
	}
	segs := strings.Split(strings.TrimPrefix(s, scheme), "/")
	var id Identity
	switch {
	case segs[0] == "worker" && len(segs) == 3:
		id = Identity{Role: RoleWorker, Pool: segs[1], InstanceID: segs[2]}
	case segs[0] == "worker" && len(segs) == 4:
		id = Identity{Role: RoleVM, Pool: segs[1], Serial: segs[2], VM: segs[3]}
	case segs[0] == "host" && len(segs) == 2:
		id = Identity{Role: RoleHost, Serial: segs[1]}
	case segs[0] == "controller" && len(segs) == 1:
		id = Identity{Role: RoleController}
	case segs[0] == "server" && len(segs) == 2:
		id = Identity{Role: RoleServer, Component: segs[1]}
	default:
		return Identity{}, fmt.Errorf("%w: %q has an unknown role or segment count", ErrInvalidIdentity, s)
	}
	if err := id.Validate(); err != nil {
		return Identity{}, err
	}
	if id.String() != s {
		return Identity{}, fmt.Errorf("%w: %q is not in canonical form", ErrInvalidIdentity, s)
	}
	return id, nil
}

// ParseIdentityURL parses a URI SAN from a certificate. It rejects every URL
// component a Cucina identity never has before parsing the string form that
// Buildbarn also sees (url.URL.String).
func ParseIdentityURL(u *url.URL) (Identity, error) {
	if u == nil {
		return Identity{}, fmt.Errorf("%w: nil URI", ErrInvalidIdentity)
	}
	if u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawFragment != "" || u.RawPath != "" || u.Port() != "" || u.OmitHost {
		return Identity{}, fmt.Errorf("%w: URI %q has components a Cucina identity never has", ErrInvalidIdentity, u.String())
	}
	return ParseIdentity(u.String())
}
