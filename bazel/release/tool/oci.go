// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OCI image-spec media types used by rules_oci layouts.
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
	MediaType string          `json:"mediaType"`
	Manifests []ociDescriptor `json:"manifests"`
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
