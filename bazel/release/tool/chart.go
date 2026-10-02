// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Path of the controller image fields in the chart's values.yaml (charts/cucina: "the release
// workflow fills in the digest").
var controllerImagePath = []string{"images", "controller"}

// SetControllerImage pins images.controller.{repository,tag,digest} in values.yaml to the image
// the release publishes. It edits only those scalars in place (comments and layout are kept)
// and then proves that nothing else changed by comparing the decoded documents.
func SetControllerImage(values []byte, repository, tag, digest string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(values, &doc); err != nil {
		return nil, fmt.Errorf("values.yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("values.yaml: expected one YAML document")
	}
	node := doc.Content[0]
	for _, key := range controllerImagePath {
		if node = mappingValue(node, key); node == nil {
			return nil, fmt.Errorf("values.yaml: no %s", strings.Join(controllerImagePath, "."))
		}
	}
	set := map[string]string{"repository": repository, "tag": tag}
	if digest != "" {
		set["digest"] = digest
	}
	lines := strings.Split(string(values), "\n")
	for _, key := range sortedKeys(set) {
		v := mappingValue(node, key)
		if v == nil || v.Kind != yaml.ScalarNode || v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return nil, fmt.Errorf("values.yaml: %s.%s is not a one-line scalar", strings.Join(controllerImagePath, "."), key)
		}
		i := v.Line - 1
		if i < 0 || i >= len(lines) || v.Column-1 > len(lines[i]) {
			return nil, fmt.Errorf("values.yaml: bad position for %s", key)
		}
		line := lines[i][:v.Column-1] + strconv.Quote(set[key])
		if v.LineComment != "" {
			line += " " + v.LineComment
		}
		lines[i] = line
	}
	out := []byte(strings.Join(lines, "\n"))

	// Proof: the result decodes to the original with exactly these fields replaced.
	var want, got map[string]any
	if err := yaml.Unmarshal(values, &want); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("values.yaml after edit: %w", err)
	}
	m := want
	for _, key := range controllerImagePath {
		next, ok := m[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("values.yaml: %s is not a mapping", key)
		}
		m = next
	}
	for k, v := range set {
		m[k] = v
	}
	if !reflect.DeepEqual(want, got) {
		return nil, errors.New("values.yaml: the edit changed more than the controller image (unexpected layout)")
	}
	return out, nil
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// OCIIndexDigest returns the digest of the single image index in an OCI image layout
// (what `crane push` publishes, and what the chart pins).
func OCIIndexDigest(layout string) (string, error) {
	var idx ociIndex
	data, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return "", fmt.Errorf("%s/index.json: %w", layout, err)
	}
	if len(idx.Manifests) != 1 || idx.Manifests[0].MediaType != mediaTypeIndex {
		return "", fmt.Errorf("%s: expected exactly one image index in the layout", layout)
	}
	return idx.Manifests[0].Digest, nil
}

// NormalizeTarGz rewrites a .tgz with fixed timestamps and owners (helm stamps the current
// time), so the chart package is reproducible.
func NormalizeTarGz(data []byte, mtime int64) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var buf bytes.Buffer
	ogz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(ogz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		nh := &tar.Header{
			Typeflag: h.Typeflag, Name: h.Name, Linkname: h.Linkname, Mode: h.Mode, Size: h.Size,
			ModTime: unixTime(mtime),
		}
		if err := tw.WriteHeader(nh); err != nil {
			return nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := ogz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadChartPackage returns the decoded Chart.yaml and values.yaml of a chart archive.
func ReadChartPackage(data []byte) (chart, values map[string]any, err error) {
	entries, err := readTarGz(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		dir, file, ok := strings.Cut(e.Name, "/")
		if !ok || strings.Contains(file, "/") || dir == "" {
			continue
		}
		switch file {
		case "Chart.yaml":
			err = yaml.Unmarshal(e.Data, &chart)
		case "values.yaml":
			err = yaml.Unmarshal(e.Data, &values)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", e.Name, err)
		}
	}
	if chart == nil || values == nil {
		return nil, nil, errors.New("chart archive without Chart.yaml or values.yaml")
	}
	return chart, values, nil
}

func runChart(args []string, _ io.Writer) error {
	fs := newFlags("chart")
	buildinfo := fs.String("buildinfo", "", "build info JSON")
	helm := fs.String("helm", "", "helm binary")
	root := fs.String("chart-root", "", "path prefix of the chart's files (e.g. charts/cucina)")
	layout := fs.String("image-layout", "", "OCI layout of the controller image (pins its digest)")
	out := fs.String("out", "", "output chart package")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required(fs, "buildinfo", "helm", "chart-root", "out"); err != nil {
		return err
	}
	bi, err := LoadBuildInfo(*buildinfo)
	if err != nil {
		return err
	}
	digest := ""
	if *layout != "" {
		if digest, err = OCIIndexDigest(*layout); err != nil {
			return err
		}
	}
	tmp, err := os.MkdirTemp("", "cucina-chart-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	stage := filepath.Join(tmp, "chart", filepath.Base(*root))
	prefix := strings.TrimSuffix(*root, "/") + "/"
	for _, f := range fs.Args() {
		rel, ok := strings.CutPrefix(filepath.ToSlash(f), prefix)
		if !ok {
			return fmt.Errorf("%s is not under --chart-root %s", f, *root)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if rel == "values.yaml" {
			if data, err = SetControllerImage(data, ImageRef(bi, "cucina-controller"), bi.Version, digest); err != nil {
				return err
			}
		}
		dst := filepath.Join(stage, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	dest := filepath.Join(tmp, "out")
	helmPath, err := filepath.Abs(*helm)
	if err != nil {
		return err
	}
	cmd := exec.Command(helmPath, "package", stage, "--version", bi.Version, "--app-version", bi.Version, "--destination", dest)
	cmd.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(tmp, "helm", "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(tmp, "helm", "config"),
		"HELM_DATA_HOME="+filepath.Join(tmp, "helm", "data"),
		"SOURCE_DATE_EPOCH="+strconv.FormatInt(bi.Epoch, 10),
	)
	if outb, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("helm package: %w\n%s", err, outb)
	}
	pkgs, err := filepath.Glob(filepath.Join(dest, "*.tgz"))
	if err != nil || len(pkgs) != 1 {
		return fmt.Errorf("helm package produced %d archives", len(pkgs))
	}
	data, err := os.ReadFile(pkgs[0])
	if err != nil {
		return err
	}
	if data, err = NormalizeTarGz(data, bi.Epoch); err != nil {
		return err
	}
	return os.WriteFile(*out, data, 0o644)
}
