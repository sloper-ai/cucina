// SPDX-License-Identifier: FSL-1.1-ALv2

// Package crds embeds the generated CustomResourceDefinition manifests so that the
// controller can apply them on `helm upgrade` (Helm's crds/ directory is install-only).
// Regenerate the YAML with controller-gen (docs/contracts.md §8).
package crds

import "embed"

// FS holds *.yaml, one CRD per file.
//
//go:embed *.yaml
var FS embed.FS
