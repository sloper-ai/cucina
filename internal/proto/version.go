// SPDX-License-Identifier: FSL-1.1-ALv2

// Package proto holds the protocol-version handshake constants shared by
// cucina-controller, cucina-hostd and cucina-worker-agent (R-TEST-7 "a
// protocol-version handshake between hostd and the controller"). The wire type
// is cucina.v1.ProtocolVersion (api/proto/cucina/v1/common.proto); cucinactl
// mirrors these numbers in cli/cucina-api.
//
// Compatibility rule: peers with the same Major interoperate; a Minor bump only
// adds optional fields/RPCs that an older peer ignores. A Major mismatch is
// refused with a precise message (see Check).
package proto

import "fmt"

const (
	// Major is bumped on incompatible changes of the host stream, enrollment or
	// management protocol.
	Major uint32 = 1
	// Minor is bumped on backwards-compatible additions.
	Minor uint32 = 0
)

// Version is a protocol version as sent by a peer.
type Version struct {
	Major uint32
	Minor uint32
}

// Current is the version this binary speaks.
var Current = Version{Major: Major, Minor: Minor}

func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Check validates a peer's version against Current. It fails when the peer sent
// no version (major 0) or a different major version; the error names both
// versions and which side has to be upgraded.
func Check(peer Version, peerRole string) error {
	switch {
	case peer.Major == 0:
		return fmt.Errorf("%s sent no protocol version (expected %s); upgrade the %s", peerRole, Current, peerRole)
	case peer.Major < Major:
		return fmt.Errorf("%s speaks protocol %s, this controller speaks %s: upgrade the %s", peerRole, peer, Current, peerRole)
	case peer.Major > Major:
		return fmt.Errorf("%s speaks protocol %s, this controller speaks %s: upgrade the controller (helm upgrade)", peerRole, peer, Current)
	}
	return nil
}
