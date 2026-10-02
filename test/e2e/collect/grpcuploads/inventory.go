// SPDX-License-Identifier: FSL-1.1-ALv2

package grpcuploads

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// InventoryFromRules converts `bazel mod show_repo --all_repos` into explicit
// toolchain/source identities. Version is the resolved immutable source rule
// revision (not guessed from canonical repo names). Only repositories owned
// by the pinned LLVM/windows_support toolchain modules are assessed; unused
// variants remain catalog entries, not required evidence. This export performs
// no repository upload and must be captured before the first remote build.
func InventoryFromRules(text string) (Inventory, error) {
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n")
	var inv Inventory
	inv.SchemaVersion = 1
	heading := regexp.MustCompile(`(?m)^## @@([^:\s]+):\s*$`)
	matches := heading.FindAllStringSubmatchIndex(text, -1)
	blocks := map[string]string{}
	for i, m := range matches {
		end := len(text)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		blocks[text[m[2]:m[3]]] = strings.TrimSpace(text[m[1]:end])
	}
	for i, m := range matches {
		name := text[m[2]:m[3]]
		component := ""
		switch {
		case name == "llvm+" || strings.HasPrefix(name, "llvm++"):
			component = "hermetic-llvm"
		case name == "windows_support+" || strings.HasPrefix(name, "windows_support++"):
			component = "windows-sdk-crt"
		case strings.HasPrefix(name, "cucina_platforms++apple+"):
			component = "exec-side-apple-sdk"
		}
		if component == "" {
			continue
		}
		end := len(text)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		block := strings.TrimSpace(text[m[1]:end])
		if !strings.Contains(block, "name = \""+name+"\"") {
			return inv, fmt.Errorf("resolved repository rule name mismatch")
		}
		// The full rule definition includes immutable download hashes, pinned
		// module labels and patch sets. Hash it as a variant/source revision;
		// do not pretend this is LLVM's human marketing version.
		owner := "llvm+"
		if component == "windows-sdk-crt" {
			owner = "windows_support+"
		}
		if component == "exec-side-apple-sdk" {
			owner = "cucina_platforms+"
		}
		ownerBlock := blocks[owner]
		if ownerBlock == "" {
			return inv, fmt.Errorf("toolchain owner module rule is missing")
		}
		// Include the owning module's immutable download/patch definition;
		// extension rules often name only @@llvm+ labels, not its version.
		d := sha256.Sum256([]byte(ownerBlock + "\n" + block))
		pin := "resolved-rule-sha256:" + hex.EncodeToString(d[:])
		version := pin
		for _, pattern := range []string{`(?:msvc_version|windows_sdk_version) = "([^"]+)"`, `releases/download/([^/\"]+)/`, `remote_module_file_urls = \["https://bcr\.bazel\.build/modules/[^/]+/([^/]+)/`} {
			if m := regexp.MustCompile(pattern).FindStringSubmatch(block); len(m) > 1 {
				version = m[1]
				break
			}
		}
		inv.Repositories = append(inv.Repositories, Repository{Name: name, Component: component, Version: version, Variant: name, Pin: pin})
	}
	if len(inv.Repositories) == 0 {
		return inv, fmt.Errorf("no pinned toolchain repository definitions in Bazel export")
	}
	sort.Slice(inv.Repositories, func(i, j int) bool { return inv.Repositories[i].Name < inv.Repositories[j].Name })
	return inv, inv.Validate()
}

// VerifyRepositoryPins checks actually exercised client repositories against
// the declared inventory. A supplied fingerprint alone is not proof that the
// client resolved those sources. Unused catalog variants are not a claim.
func VerifyRepositoryPins(expected, actual Inventory, names []string) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if err := actual.Validate(); err != nil {
		return err
	}
	ex, ac := map[string]Repository{}, map[string]Repository{}
	for _, r := range expected.Repositories {
		ex[r.Name] = r
	}
	for _, r := range actual.Repositories {
		ac[r.Name] = r
	}
	for _, name := range names {
		want, wok := ex[name]
		got, gok := ac[name]
		if !wok || !gok || want != got {
			return fmt.Errorf("client resolved repository pin differs or is absent for %s", name)
		}
	}
	return nil
}

func isToolchainRepo(name string) bool {
	return name == "llvm+" || strings.HasPrefix(name, "llvm++") || name == "windows_support+" || strings.HasPrefix(name, "windows_support++") || strings.HasPrefix(name, "cucina_platforms++apple+")
}
