// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OCI image-spec media types used by exported image layouts.
const (
	mediaTypeIndex    = "application/vnd.oci.image.index.v1+json"
	mediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
)

type ociDescriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  *ociPlatform      `json:"platform,omitempty"`
	Annots    map[string]string `json:"annotations,omitempty"`
}

type ociPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

type ociManifest struct {
	MediaType string          `json:"mediaType"`
	Config    ociDescriptor   `json:"config"`
	Layers    []ociDescriptor `json:"layers"`
}

type ociConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Created      string `json:"created"`
	Config       struct {
		User       string            `json:"User"`
		Entrypoint []string          `json:"Entrypoint"`
		Labels     map[string]string `json:"Labels"`
	} `json:"config"`
}

// runWrapOCI preserves the single-index envelope consumed by release packaging
// and offline publication tools. The rules_img index and all payload blobs remain
// byte-identical; only the OCI layout's entry-point index.json is replaced.
func runWrapOCI(args []string, _ io.Writer) error {
	fs := newFlags("wrap-oci")
	source := fs.String("layout", "", "complete rules_img OCI layout with a flat index")
	out := fs.String("out", "", "output OCI layout with a single index descriptor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required(fs, "layout", "out"); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(*source, "index.json"))
	if err != nil {
		return err
	}
	var index ociIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return err
	}
	if index.SchemaVersion != 2 || index.MediaType != mediaTypeIndex || len(index.Manifests) == 0 {
		return fmt.Errorf("expected a nonempty OCI image index")
	}
	for _, manifest := range index.Manifests {
		if manifest.MediaType != mediaTypeManifest {
			return fmt.Errorf("expected platform image manifests, got %q", manifest.MediaType)
		}
	}
	// Sandbox input trees contain symlinks to immutable action outputs. Materialize
	// their contents; CopyFS would preserve the links and make this export depend
	// on (or overwrite) the original tree. OCI payloads here are SHA-256 blobs.
	blobs, err := listFiles(filepath.Join(*source, "blobs", "sha256"))
	if err != nil {
		return err
	}
	outBlobs := filepath.Join(*out, "blobs", "sha256")
	if err := os.MkdirAll(outBlobs, 0o755); err != nil {
		return err
	}
	for _, name := range blobs {
		payload, err := readBlob(*source, "sha256:"+name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outBlobs, name), payload, 0o644); err != nil {
			return err
		}
	}
	if err := copyFile(filepath.Join(*source, "oci-layout"), filepath.Join(*out, "oci-layout"), 0o644); err != nil {
		return err
	}
	hexsum := fmt.Sprintf("%x", sha256.Sum256(data))
	blobPath := filepath.Join(*out, "blobs", "sha256", hexsum)
	if err := os.WriteFile(blobPath, data, 0o644); err != nil {
		return err
	}
	return writeJSON(filepath.Join(*out, "index.json"), ociIndex{
		SchemaVersion: 2,
		MediaType:     mediaTypeIndex,
		Manifests: []ociDescriptor{{
			MediaType: mediaTypeIndex,
			Digest:    "sha256:" + hexsum,
			Size:      int64(len(data)),
		}},
	})
}

// ImagePlatform is one platform image of a multi-arch image read from an OCI layout.
type ImagePlatform struct {
	Platform string // os/arch
	Config   ociConfig
	Files    map[string][]byte // regular files of all layers by absolute path (only those asked for)
}

func unixTime(epoch int64) time.Time { return time.Unix(epoch, 0).UTC() }

// readBlob reads a content-addressed blob of a layout and checks its digest.
func readBlob(layout, digest string) ([]byte, error) {
	algo, hexsum, ok := strings.Cut(digest, ":")
	if !ok || algo != "sha256" || len(hexsum) != 64 {
		return nil, fmt.Errorf("unsupported digest %q", digest)
	}
	data, err := os.ReadFile(filepath.Join(layout, "blobs", algo, hexsum))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hexsum {
		return nil, fmt.Errorf("blob %s: content does not match its digest", digest)
	}
	return data, nil
}

func readJSONBlob(layout, digest string, v any) error {
	data, err := readBlob(layout, digest)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// ReadImageLayout reads every platform image of the image index in an OCI layout, keeping
// the content of the files named in want (absolute paths inside the image).
func ReadImageLayout(layout string, want []string) (digest string, images []ImagePlatform, err error) {
	if digest, err = OCIIndexDigest(layout); err != nil {
		return "", nil, err
	}
	var idx ociIndex
	if err := readJSONBlob(layout, digest, &idx); err != nil {
		return "", nil, err
	}
	for _, d := range idx.Manifests {
		if d.MediaType != mediaTypeManifest || d.Platform == nil {
			return "", nil, fmt.Errorf("index entry %s is not a platform image manifest", d.Digest)
		}
		var m ociManifest
		if err := readJSONBlob(layout, d.Digest, &m); err != nil {
			return "", nil, err
		}
		img := ImagePlatform{Platform: d.Platform.OS + "/" + d.Platform.Architecture, Files: map[string][]byte{}}
		if err := readJSONBlob(layout, m.Config.Digest, &img.Config); err != nil {
			return "", nil, err
		}
		for _, l := range m.Layers {
			data, err := readBlob(layout, l.Digest)
			if err != nil {
				return "", nil, err
			}
			if bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
				gz, err := gzip.NewReader(bytes.NewReader(data))
				if err != nil {
					return "", nil, err
				}
				entries, err := readTar(gz)
				if err != nil {
					return "", nil, fmt.Errorf("layer %s: %w", l.Digest, err)
				}
				addFiles(img.Files, entries, want)
				continue
			}
			if !strings.Contains(l.MediaType, "tar") || strings.Contains(l.MediaType, "zstd") {
				return "", nil, fmt.Errorf("layer %s: unsupported media type %s", l.Digest, l.MediaType)
			}
			entries, err := readTar(bytes.NewReader(data))
			if err != nil {
				return "", nil, fmt.Errorf("layer %s: %w", l.Digest, err)
			}
			addFiles(img.Files, entries, want)
		}
		images = append(images, img)
	}
	return digest, images, nil
}

func addFiles(files map[string][]byte, entries []ArchiveEntry, want []string) {
	for _, e := range entries {
		p := "/" + strings.TrimPrefix(e.Name, "/")
		for _, w := range want {
			if p == w && e.Data != nil {
				files[p] = e.Data
			}
		}
	}
}
