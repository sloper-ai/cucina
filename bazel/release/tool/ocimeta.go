// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Licence and vendor of Cucina's own artifacts (LICENSE.md).
const (
	license = "FSL-1.1-ALv2"
	vendor  = "Brwse Co."
)

// ImageLabels returns the org.opencontainers.image.* labels of a release image
// (https://github.com/opencontainers/image-spec/blob/main/annotations.md).
func ImageLabels(bi BuildInfo, title, description, baseName, baseDigest string) map[string]string {
	repoURL := "https://github.com/" + bi.Repository
	labels := map[string]string{
		"org.opencontainers.image.title":         title,
		"org.opencontainers.image.description":   description,
		"org.opencontainers.image.version":       bi.Version,
		"org.opencontainers.image.created":       bi.Created().Format(time.RFC3339),
		"org.opencontainers.image.source":        repoURL,
		"org.opencontainers.image.url":           repoURL,
		"org.opencontainers.image.documentation": repoURL + "/blob/" + bi.Tag() + "/docs/operations/releasing.md",
		"org.opencontainers.image.licenses":      license,
		"org.opencontainers.image.vendor":        vendor,
		"ai.sloper.cucina.source-dirty":          strconv.FormatBool(bi.Dirty),
	}
	if bi.Commit != "" {
		labels["org.opencontainers.image.revision"] = bi.Commit
	}
	if baseName != "" {
		labels["org.opencontainers.image.base.name"] = baseName
	}
	if baseDigest != "" {
		labels["org.opencontainers.image.base.digest"] = baseDigest
	}
	return labels
}

func runOCIMeta(args []string, _ io.Writer) error {
	fs := newFlags("oci-meta")
	buildinfo := fs.String("buildinfo", "", "build info JSON")
	title := fs.String("title", "", "image title (the published repository name)")
	description := fs.String("description", "", "image description")
	repository := fs.String("repository", "", "repository name below ghcr.io/<owner>/ (for repo tags)")
	baseName := fs.String("base-name", "", "base image repository")
	baseLayout := fs.String("base-layout", "", "OCI layout of the base image (its manifest digest)")
	outLabels := fs.String("out-labels", "", "output: name=value label lines (rules_oci `labels`)")
	outCreated := fs.String("out-created", "", "output: RFC 3339 creation time (rules_oci `created`)")
	outTags := fs.String("out-tags", "", "output: remote tags, one per line")
	outRepoTags := fs.String("out-repo-tags", "", "output: <registry>/<owner>/<repository>:<version> (oci_load)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required(fs, "buildinfo", "title", "description", "out-labels", "out-created", "out-tags"); err != nil {
		return err
	}
	bi, err := LoadBuildInfo(*buildinfo)
	if err != nil {
		return err
	}
	baseDigest := ""
	if *baseLayout != "" {
		if baseDigest, err = layoutManifestDigest(*baseLayout); err != nil {
			return err
		}
	}
	labels := ImageLabels(bi, *title, *description, *baseName, baseDigest)
	keys := make([]string, 0, len(labels))
	for k, v := range labels {
		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("label %s contains a newline", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, labels[k])
	}
	if err := os.WriteFile(*outLabels, []byte(b.String()), 0o644); err != nil {
		return err
	}
	// rules_oci reads `created` with jq --rawfile: no trailing newline.
	if err := os.WriteFile(*outCreated, []byte(bi.Created().Format(time.RFC3339)), 0o644); err != nil {
		return err
	}
	// One immutable tag per release; the chart pins the digest (no moving `latest`).
	if err := os.WriteFile(*outTags, []byte(bi.Version+"\n"), 0o644); err != nil {
		return err
	}
	if *outRepoTags == "" {
		return nil
	}
	if *repository == "" {
		return fmt.Errorf("--out-repo-tags needs --repository")
	}
	return os.WriteFile(*outRepoTags, []byte(ImageRef(bi, *repository)+":"+bi.Version+"\n"), 0o644)
}

// ImageRef is where the release workflow publishes an image (R-OPS-7: public GHCR).
func ImageRef(bi BuildInfo, repository string) string {
	return "ghcr.io/" + bi.Owner() + "/" + repository
}

// layoutManifestDigest is the digest of the single manifest (image or index) of a layout.
func layoutManifestDigest(layout string) (string, error) {
	var idx ociIndex
	data, err := os.ReadFile(layout + "/index.json")
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return "", err
	}
	if len(idx.Manifests) != 1 {
		return "", fmt.Errorf("%s: %d manifests, want 1", layout, len(idx.Manifests))
	}
	return idx.Manifests[0].Digest, nil
}
