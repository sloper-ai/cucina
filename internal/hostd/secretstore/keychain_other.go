// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !darwin || !cgo

package secretstore

import (
	"context"
	"errors"

	"github.com/sloper-ai/cucina/internal/ports"
)

// KeychainAvailable reports whether the keychain store is compiled in.
const KeychainAvailable = false

var errNoKeychain = errors.New("secretstore: the macOS keychain is not available in this build")

// Keychain is unavailable outside darwin+cgo builds.
type Keychain struct {
	Service string
}

var _ ports.SecretStore = Keychain{}

// Get implements ports.SecretStore.
func (Keychain) Get(context.Context, string) ([]byte, error) { return nil, errNoKeychain }

// Put implements ports.SecretStore.
func (Keychain) Put(context.Context, string, []byte) error { return errNoKeychain }

// Delete implements ports.SecretStore.
func (Keychain) Delete(context.Context, string) error { return errNoKeychain }
