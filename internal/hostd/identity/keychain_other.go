// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !darwin || !cgo

package identity

import (
	"crypto/tls"
	"errors"
)

// KeychainAvailable reports whether keychain identities can be read.
const KeychainAvailable = false

// KeychainIdentity is unavailable outside darwin+cgo builds.
func KeychainIdentity(string) (*tls.Certificate, error) {
	return nil, errors.New("keychain identities need a darwin cgo build")
}
