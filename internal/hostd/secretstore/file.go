// SPDX-License-Identifier: FSL-1.1-ALv2

// Package secretstore implements ports.SecretStore for hostd: the macOS System
// keychain for the root daemon (keychain_darwin.go, R-SEC-3 "the key stays in
// the System keychain") and a 0600-file fallback for user mode and tests.
package secretstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/sloper-ai/cucina/internal/ports"
)

var keyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func checkKey(key string) error {
	if !keyRe.MatchString(key) {
		return fmt.Errorf("secretstore: invalid key %q", key)
	}
	return nil
}

// File stores each secret in Dir/<key> with mode 0600 (Dir is created 0700).
type File struct {
	Dir string
}

var _ ports.SecretStore = File{}

// Get implements ports.SecretStore.
func (f File) Get(_ context.Context, key string) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(f.Dir, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ports.ErrNotFound
	}
	return b, err
}

// Put implements ports.SecretStore.
func (f File) Put(_ context.Context, key string, value []byte) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(f.Dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.Dir, "."+key+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(value); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(f.Dir, key))
}

// Delete implements ports.SecretStore; deleting a missing key succeeds.
func (f File) Delete(_ context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(f.Dir, key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
