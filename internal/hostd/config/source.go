// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"

	"howett.net/plist"
)

// PlistSource serves preference values from one parsed plist dictionary (XML,
// binary or OpenStep). It is the fallback when CFPreferences is unavailable
// (user mode, tests, non-darwin builds) and the unknown-key check of the
// managed file in root mode.
type PlistSource struct {
	values map[string]any
	forced bool
}

// ParsePlist parses a preference plist. forced marks every value as managed
// (true for files under /Library/Managed Preferences).
func ParsePlist(data []byte, forced bool) (*PlistSource, error) {
	values := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		if _, err := plist.Unmarshal(data, &values); err != nil {
			return nil, &Error{Msg: fmt.Sprintf("preferences are not a plist dictionary: %v", err)}
		}
	}
	return &PlistSource{values: values, forced: forced}, nil
}

// ReadPlistFile reads a preference plist; a missing file yields an empty source.
func ReadPlistFile(path string, forced bool) (*PlistSource, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &PlistSource{values: map[string]any{}, forced: forced}, nil
	}
	if err != nil {
		return nil, &Error{Msg: fmt.Sprintf("reading %s: %v", path, err)}
	}
	return ParsePlist(data, forced)
}

// Value implements Source.
func (p *PlistSource) Value(key string) (any, bool, bool) {
	v, ok := p.values[key]
	return v, p.forced && ok, ok
}

// Keys implements Source.
func (p *PlistSource) Keys() []string {
	keys := make([]string, 0, len(p.values))
	for k := range p.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Layered resolves each key from the first source that has it (CFPreferences
// order: forced/managed first, then local preferences). Keys is the union.
type Layered []Source

// Value implements Source.
func (l Layered) Value(key string) (any, bool, bool) {
	for _, s := range l {
		if v, forced, ok := s.Value(key); ok {
			return v, forced, true
		}
	}
	return nil, false, false
}

// Keys implements Source.
func (l Layered) Keys() []string {
	seen := map[string]bool{}
	var keys []string
	for _, s := range l {
		for _, k := range s.Keys() {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}
