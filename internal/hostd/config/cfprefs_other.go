// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !darwin || !cgo

package config

// CFAvailable reports whether the CFPreferences reader is compiled in.
const CFAvailable = false

// NewCFSource falls back to the key source (the managed plist file) when
// CFPreferences is unavailable (non-darwin or CGO_ENABLED=0 builds).
func NewCFSource(_ string, keys Source) Source {
	if keys == nil {
		return Layered(nil)
	}
	return keys
}
