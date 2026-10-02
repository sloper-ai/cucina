// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package secretstore

import (
	"context"
	"errors"

	"github.com/keybase/go-keychain"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Keychain stores secrets as generic-password items (service = Service,
// account = key). Running as root, the SecItem APIs use root's default
// keychain, the file-based System keychain (/Library/Keychains/System.keychain),
// which daemons can use (TN3137); the data-protection keychain is not used.
type Keychain struct {
	Service string // "ai.sloper.cucina.hostd"
}

// KeychainAvailable reports whether the keychain store is compiled in.
const KeychainAvailable = true

var _ ports.SecretStore = Keychain{}

// Get implements ports.SecretStore.
func (k Keychain) Get(_ context.Context, key string) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	b, err := keychain.GetGenericPassword(k.Service, key, "", "")
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, ports.ErrNotFound
	}
	return b, nil
}

// Put implements ports.SecretStore (add, or update when present).
func (k Keychain) Put(_ context.Context, key string, value []byte) error {
	if err := checkKey(key); err != nil {
		return err
	}
	item := keychain.NewGenericPassword(k.Service, key, "Cucina host agent ("+key+")", value, "")
	item.SetSynchronizable(keychain.SynchronizableNo)
	err := keychain.AddItem(item)
	if errors.Is(err, keychain.ErrorDuplicateItem) {
		query := keychain.NewItem()
		query.SetSecClass(keychain.SecClassGenericPassword)
		query.SetService(k.Service)
		query.SetAccount(key)
		update := keychain.NewItem()
		update.SetData(value)
		return keychain.UpdateItem(query, update)
	}
	return err
}

// Delete implements ports.SecretStore; deleting a missing key succeeds.
func (k Keychain) Delete(_ context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	err := keychain.DeleteGenericPasswordItem(k.Service, key)
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return nil
	}
	return err
}
