// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"compress/bzip2"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// A macOS product archive (.pkg) is a xar archive: a fixed header, a zlib-compressed XML table
// of contents and a heap. This reader extracts top-level files (the Distribution) and the
// signing certificate so release checks also run on Linux, without pkgutil.

const xarMagic = 0x78617221 // "xar!"

type xarHeader struct {
	Magic            uint32
	Size             uint16
	Version          uint16
	TOCCompressed    uint64
	TOCUncompressed  uint64
	ChecksumAlgorith uint32
}

type xarData struct {
	Length   int64 `xml:"length"`
	Offset   int64 `xml:"offset"`
	Size     int64 `xml:"size"`
	Encoding struct {
		Style string `xml:"style,attr"`
	} `xml:"encoding"`
}

type xarFile struct {
	Name  string    `xml:"name"`
	Type  string    `xml:"type"`
	Data  *xarData  `xml:"data"`
	Files []xarFile `xml:"file"`
}

type xarTOC struct {
	Files        []xarFile `xml:"toc>file"`
	Certificates []string  `xml:"toc>signature>KeyInfo>X509Data>X509Certificate"`
}

// Pkg is what release checks need from a product archive.
type Pkg struct {
	Distribution []byte
	SignerSHA1   string // SHA-1 of the leaf signing certificate (DER), "" when unsigned
}

// ReadPkg parses a product archive.
func ReadPkg(data []byte) (Pkg, error) {
	var h xarHeader
	if err := binary.Read(bytes.NewReader(data), binary.BigEndian, &h); err != nil {
		return Pkg{}, fmt.Errorf("xar header: %w", err)
	}
	if h.Magic != xarMagic {
		return Pkg{}, errors.New("not a xar archive (product .pkg)")
	}
	tocEnd := int64(h.Size) + int64(h.TOCCompressed)
	if int64(h.Size) < 28 || tocEnd > int64(len(data)) {
		return Pkg{}, errors.New("xar: truncated table of contents")
	}
	zr, err := zlib.NewReader(bytes.NewReader(data[h.Size:tocEnd]))
	if err != nil {
		return Pkg{}, fmt.Errorf("xar toc: %w", err)
	}
	tocXML, err := io.ReadAll(io.LimitReader(zr, 16<<20))
	if err != nil {
		return Pkg{}, fmt.Errorf("xar toc: %w", err)
	}
	var toc xarTOC
	if err := xml.Unmarshal(tocXML, &toc); err != nil {
		return Pkg{}, fmt.Errorf("xar toc: %w", err)
	}
	heap := data[tocEnd:]
	var pkg Pkg
	for _, f := range toc.Files {
		if f.Name != "Distribution" || f.Data == nil {
			continue
		}
		if pkg.Distribution, err = xarExtract(heap, f.Data); err != nil {
			return Pkg{}, fmt.Errorf("xar Distribution: %w", err)
		}
	}
	if pkg.Distribution == nil {
		return Pkg{}, errors.New("product archive without a Distribution file")
	}
	if len(toc.Certificates) > 0 {
		der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(toc.Certificates[0]), ""))
		if err != nil {
			return Pkg{}, fmt.Errorf("xar signature certificate: %w", err)
		}
		sum := sha1.Sum(der) // names the certificate as Apple tools do (anchor = H"…")
		pkg.SignerSHA1 = strings.ToUpper(hex.EncodeToString(sum[:]))
	}
	return pkg, nil
}

func xarExtract(heap []byte, d *xarData) ([]byte, error) {
	if d.Offset < 0 || d.Length < 0 || d.Offset+d.Length > int64(len(heap)) {
		return nil, errors.New("data outside the heap")
	}
	raw := heap[d.Offset : d.Offset+d.Length]
	var r io.Reader
	switch d.Encoding.Style {
	case "", "application/octet-stream":
		return raw, nil
	case "application/x-gzip":
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		r = zr
	case "application/x-bzip2":
		r = bzip2.NewReader(bytes.NewReader(raw))
	default:
		return nil, fmt.Errorf("unsupported encoding %q", d.Encoding.Style)
	}
	return io.ReadAll(io.LimitReader(r, 64<<20))
}

// distribution is the part of a productbuild Distribution file the checks read.
type distribution struct {
	Product struct {
		ID      string `xml:"id,attr"`
		Version string `xml:"version,attr"`
	} `xml:"product"`
	PkgRefs []struct {
		ID      string `xml:"id,attr"`
		Version string `xml:"version,attr"`
	} `xml:"pkg-ref"`
}

// PkgVersions returns the product version and the versions of every pkg-ref with id.
func PkgVersions(dist []byte, id string) (product string, refs []string, err error) {
	var d distribution
	if err := xml.Unmarshal(dist, &d); err != nil {
		return "", nil, fmt.Errorf("distribution: %w", err)
	}
	if d.Product.ID != id {
		return "", nil, fmt.Errorf("distribution: product id %q, want %q", d.Product.ID, id)
	}
	for _, r := range d.PkgRefs {
		if r.ID == id && r.Version != "" {
			refs = append(refs, r.Version)
		}
	}
	return d.Product.Version, refs, nil
}
