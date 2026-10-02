// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// ArchiveSpec describes one CLI archive: a top-level directory holding the executable, its
// argv[0] aliases and the licence documents.
type ArchiveSpec struct {
	Top   string            // top-level directory inside the archive
	Exe   string            // executable name, e.g. cucinactl or cucinactl.exe
	Links []string          // aliases of Exe dispatched on argv[0] (R-AUTH-8)
	Docs  map[string]string // name -> path, mode 0644
	Time  time.Time         // modification time of every entry
}

// WriteTarGz writes the archive with every alias as a hard link to the executable, so
// `tar x` recreates them as hard links (R-AUTH-8: Bazel runs the helper path without
// arguments, so the alias must exist as a file).
func WriteTarGz(w io.Writer, spec ArchiveSpec, exe []byte, docs map[string][]byte) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	// Deterministic gzip header: no name, no timestamp.
	gz.ModTime = time.Time{}
	tw := tar.NewWriter(gz)
	hdr := func(name string, mode int64, size int64, typ byte) *tar.Header {
		return &tar.Header{
			Typeflag: typ, Name: name, Mode: mode, Size: size,
			ModTime: spec.Time, Uid: 0, Gid: 0, Uname: "root", Gname: "root",
		}
	}
	if err := tw.WriteHeader(hdr(spec.Top+"/", 0o755, 0, tar.TypeDir)); err != nil {
		return err
	}
	exePath := spec.Top + "/" + spec.Exe
	if err := tw.WriteHeader(hdr(exePath, 0o755, int64(len(exe)), tar.TypeReg)); err != nil {
		return err
	}
	if _, err := tw.Write(exe); err != nil {
		return err
	}
	for _, link := range spec.Links {
		h := hdr(spec.Top+"/"+link, 0o755, 0, tar.TypeLink)
		h.Linkname = exePath
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(docs) {
		if err := tw.WriteHeader(hdr(spec.Top+"/"+name, 0o644, int64(len(docs[name])), tar.TypeReg)); err != nil {
			return err
		}
		if _, err := tw.Write(docs[name]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// zipEpoch is the earliest time a zip (MS-DOS) timestamp can hold.
var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// WriteZip writes the archive for Windows, where aliases are copies (no hard links in zip).
func WriteZip(w io.Writer, spec ArchiveSpec, exe []byte, docs map[string][]byte) error {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestCompression)
	})
	mtime := spec.Time
	if mtime.Before(zipEpoch) {
		mtime = zipEpoch
	}
	add := func(name string, mode os.FileMode, data []byte) error {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mtime}
		h.SetMode(mode)
		f, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	}
	if err := add(spec.Top+"/"+spec.Exe, 0o755, exe); err != nil {
		return err
	}
	for _, link := range spec.Links {
		if err := add(spec.Top+"/"+link, 0o755, exe); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(docs) {
		if err := add(spec.Top+"/"+name, 0o644, docs[name]); err != nil {
			return err
		}
	}
	return zw.Close()
}

// ArchiveEntry is one file of an archive as read back by ReadArchive.
type ArchiveEntry struct {
	Name     string // path inside the archive
	Mode     os.FileMode
	Data     []byte // nil for hard links and directories
	HardLink string // target of a tar hard link
	Dir      bool
}

// ReadArchive lists a .tar.gz or .zip archive (verification only; archives are small).
func ReadArchive(name string, data []byte) ([]ArchiveEntry, error) {
	switch {
	case strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz"):
		return readTarGz(bytes.NewReader(data))
	case strings.HasSuffix(name, ".zip"):
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		var out []ArchiveEntry
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			body, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return nil, err
			}
			out = append(out, ArchiveEntry{Name: f.Name, Mode: f.Mode(), Data: body, Dir: f.FileInfo().IsDir()})
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: unknown archive type", name)
}

func readTarGz(r io.Reader) ([]ArchiveEntry, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	return readTar(gz)
}

func readTar(r io.Reader) ([]ArchiveEntry, error) {
	tr := tar.NewReader(r)
	var out []ArchiveEntry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		e := ArchiveEntry{Name: strings.TrimPrefix(h.Name, "./"), Mode: os.FileMode(h.Mode) & os.ModePerm}
		switch h.Typeflag {
		case tar.TypeDir:
			e.Dir = true
		case tar.TypeLink:
			e.HardLink = strings.TrimPrefix(h.Linkname, "./")
		case tar.TypeReg:
			if e.Data, err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		default:
			// Symlinks and others carry no content we check.
		}
		out = append(out, e)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func runArchive(args []string, _ io.Writer) error {
	fs := newFlags("archive")
	buildinfo := fs.String("buildinfo", "", "build info JSON")
	format := fs.String("format", "", "tar.gz or zip")
	top := fs.String("top", "", "top-level directory template, e.g. cucinactl-{version}-linux-amd64")
	exe := fs.String("exe", "", "NAME=PATH of the executable")
	var links, docs multiFlag
	fs.Var(&links, "link", "alias of the executable (repeated)")
	fs.Var(&docs, "doc", "NAME=PATH of a document (repeated)")
	out := fs.String("out", "", "output archive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required(fs, "buildinfo", "format", "top", "exe", "out"); err != nil {
		return err
	}
	bi, err := LoadBuildInfo(*buildinfo)
	if err != nil {
		return err
	}
	exeKV, err := pairs([]string{*exe})
	if err != nil {
		return err
	}
	exeData, err := os.ReadFile(exeKV[0][1])
	if err != nil {
		return err
	}
	docKV, err := pairs(docs)
	if err != nil {
		return err
	}
	spec := ArchiveSpec{Top: bi.Expand(*top), Exe: exeKV[0][0], Links: links, Docs: map[string]string{}, Time: bi.Created()}
	docData := map[string][]byte{}
	for _, kv := range docKV {
		if docData[kv[0]], err = os.ReadFile(kv[1]); err != nil {
			return err
		}
	}
	for _, n := range append(append([]string{spec.Top, spec.Exe}, spec.Links...), sortedKeys(docData)...) {
		if n == "" || strings.ContainsAny(n, "/\\") || path.Clean(n) != n {
			return fmt.Errorf("invalid archive name %q", n)
		}
	}
	var buf bytes.Buffer
	switch *format {
	case "tar.gz":
		err = WriteTarGz(&buf, spec, exeData, docData)
	case "zip":
		err = WriteZip(&buf, spec, exeData, docData)
	default:
		err = fmt.Errorf("unknown --format %q", *format)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(*out, buf.Bytes(), 0o644)
}
