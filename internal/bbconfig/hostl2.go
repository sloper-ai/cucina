// SPDX-License-Identifier: FSL-1.1-ALv2

package bbconfig

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"path"
	"regexp"
	"strings"
	"text/template"

	bbpath "github.com/buildbarn/bb-storage/pkg/filesystem/path"
)

// DefaultHostL2Bytes is the host L2 cache size on the Mac's SSD (R-CACHE-4).
const DefaultHostL2Bytes = 200 * GiB

// DefaultHostL2ReplicationConcurrency bounds concurrent WAN fetches of one host.
const DefaultHostL2ReplicationConcurrency = 64

// HostL2Settings are the inputs of RenderHostL2: the host-level bb_storage cache
// that cucina-hostd runs on every Mac (R-CACHE-4, R-DATA-3). VMs use it as the
// slow side of their L1; it reads through to the central worker endpoint, so
// repeated content crosses the WAN once per host. Writes, FindMissing, AC and
// FSAC traffic pass through to the central endpoint.
type HostL2Settings struct {
	// ListenAddresses are the VM-facing gRPC listeners: hostd's vmnet bridge
	// address (for example "192.168.64.1:8981"). Wildcard addresses are refused
	// (network restriction: only the host's VMs can reach the cache).
	ListenAddresses []string
	// ServerCertificatePath and ServerPrivateKeyPath hold the L2's TLS server key
	// pair (re-read every 5 minutes). VMs verify it with their CA bundle and
	// Machine.StorageServerName.
	ServerCertificatePath string
	ServerPrivateKeyPath  string
	// CABundlePEM verifies the VMs' client certificates and the central endpoint.
	CABundlePEM string
	// HostSerial, when set, admits only this host's VMs: their URI SAN is
	// spiffe://cucina/worker/<pool>/<serial>/<vm> (docs/security.md).
	HostSerial string

	// UpstreamAddress is host:port of the central worker endpoint (the
	// frontend's worker listener); UpstreamServerName its TLS server name.
	UpstreamAddress    string
	UpstreamServerName string
	// ClientCertificatePath and ClientPrivateKeyPath hold the host identity
	// (spiffe://cucina/host/<serial>) used for the upstream mTLS.
	ClientCertificatePath string
	ClientPrivateKeyPath  string
	// DisableWANCompression turns off zstd on the upstream hop (on by default:
	// it is the WAN hop of R-DATA-3).
	DisableWANCompression bool

	// CacheDir holds the cache files (blocks, key_location_map, state/) on the
	// host SSD; it must exist (with state/) and be private to hostd. It uses
	// absolute POSIX or fully qualified Windows target syntax, independently
	// of the OS running this renderer (native Buildbarn boot verification).
	CacheDir string
	// CacheSizeBytes is the blocks size; 0 means DefaultHostL2Bytes.
	CacheSizeBytes uint64
	// MaximumMessageSizeBytes must equal every other component's (R-CP-6).
	MaximumMessageSizeBytes uint64
	// ReplicationConcurrency bounds concurrent upstream fetches; 0 means
	// DefaultHostL2ReplicationConcurrency.
	ReplicationConcurrency int
	// MetricsListenAddress is the diagnostics HTTP listener hostd scrapes
	// (for example "127.0.0.1:9981"); empty disables it.
	MetricsListenAddress string
}

//go:embed hostl2.json.tmpl
var hostL2Template string

var (
	hostL2Tmpl = template.Must(template.New("hostl2").Funcs(template.FuncMap{
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
	}).Parse(hostL2Template))
	hostSerial = regexp.MustCompile(`^[A-Z0-9]{6,32}$`)
)

type hostL2Data struct {
	HostL2Settings
	Compression          bool
	ValidationExpression string
	Blocks               L1Plan
	KeyLocationMapPath   string
	BlocksPath           string
	StatePath            string
	MaximumEncoders      int
	MaximumDecoders      int
	WANEncoderLevel      int
}

// RenderHostL2 renders the host L2 bb_storage configuration in the NEW nested
// keyLocationMap schema of the pinned bb_storage (ADR 0001).
func RenderHostL2(s HostL2Settings) ([]byte, error) {
	if s.CacheSizeBytes == 0 {
		s.CacheSizeBytes = DefaultHostL2Bytes
	}
	if s.ReplicationConcurrency == 0 {
		s.ReplicationConcurrency = DefaultHostL2ReplicationConcurrency
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("invalid host L2 settings: %w", err)
	}
	blocks := hostL2Blocks(s.CacheSizeBytes)
	d := hostL2Data{
		HostL2Settings:       s,
		Compression:          !s.DisableWANCompression,
		ValidationExpression: "length(uris) == `1` && starts_with(uris[0], 'spiffe://cucina/worker/')",
		Blocks:               blocks,
		KeyLocationMapPath:   s.cachePath("key_location_map"),
		BlocksPath:           s.cachePath("blocks"),
		StatePath:            s.cachePath("state"),
		MaximumEncoders:      min(s.ReplicationConcurrency, maxZstdEncoders),
		MaximumDecoders:      min(s.ReplicationConcurrency, maxZstdDecoders),
		WANEncoderLevel:      wanEncoderLevel,
	}
	if s.HostSerial != "" {
		d.ValidationExpression += " && contains(uris[0], '/" + s.HostSerial + "/')"
	}
	var raw bytes.Buffer
	if err := hostL2Tmpl.Execute(&raw, d); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw.Bytes(), "", "  "); err != nil {
		return nil, fmt.Errorf("host L2 template produced invalid JSON: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// HostL2Directories lists the directories RenderHostL2's configuration needs
// (hostd creates them, 0700, before starting bb_storage).
func HostL2Directories(s HostL2Settings) []Directory {
	return []Directory{
		{Path: s.CacheDir, Mode: 0o700},
		{Path: s.cachePath("state"), Mode: 0o700},
	}
}

// Cache roots describe the target of the rendered configuration. filepath would
// instead interpret them according to the OS running this renderer.
func (s HostL2Settings) posixCachePath() bool {
	return path.IsAbs(s.CacheDir) && !strings.HasPrefix(s.CacheDir, "//")
}

func (s HostL2Settings) cachePath(name string) string {
	if s.posixCachePath() {
		return path.Join(s.CacheDir, name)
	}
	m := Machine{OS: OSWindows}
	return m.join(s.CacheDir, name)
}

func (s HostL2Settings) absoluteCachePath() bool {
	if s.posixCachePath() {
		return true
	}
	// The pinned parser treats a drive name as rooted. Unlike a WinFSP mount,
	// a cache directory must include the separator, never the drive's CWD.
	p := strings.ReplaceAll(s.CacheDir, "/", `\`)
	drivePath := p
	for _, prefix := range []string{`\\?\`, `\??\`, `\\.\`} {
		if strings.HasPrefix(drivePath, prefix) {
			drivePath = strings.TrimPrefix(drivePath, prefix)
			break
		}
	}
	if len(drivePath) >= 2 && drivePath[1] == ':' && (len(drivePath) < 3 || drivePath[2] != '\\') {
		return false
	}
	b, w := bbpath.EmptyBuilder.Join(bbpath.VoidScopeWalker)
	// A trailing separator permits a UNC share root as well as its children.
	if err := bbpath.Resolve(bbpath.WindowsFormat.NewParser(p+`\`), w); err != nil {
		return false
	}
	return b.WindowsPathKind() == bbpath.WindowsPathKindAbsolute
}

func (s *HostL2Settings) validate() error {
	var errs []error
	if len(s.ListenAddresses) == 0 {
		errs = append(errs, errors.New("ListenAddresses must not be empty"))
	}
	for _, a := range s.ListenAddresses {
		host, _, err := net.SplitHostPort(a)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("listen address %q: %w", a, err))
		case host == "" || net.ParseIP(host) == nil || net.ParseIP(host).IsUnspecified():
			errs = append(errs, fmt.Errorf("listen address %q must name one interface address (the VM bridge)", a))
		}
	}
	for _, f := range []struct{ name, value string }{
		{"ServerCertificatePath", s.ServerCertificatePath},
		{"ServerPrivateKeyPath", s.ServerPrivateKeyPath},
		{"UpstreamAddress", s.UpstreamAddress},
		{"ClientCertificatePath", s.ClientCertificatePath},
		{"ClientPrivateKeyPath", s.ClientPrivateKeyPath},
		{"CacheDir", s.CacheDir},
	} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("%s must be set", f.name))
		}
	}
	if s.CacheDir != "" && !s.absoluteCachePath() {
		errs = append(errs, fmt.Errorf("CacheDir %q must be absolute", s.CacheDir))
	}
	if !strings.Contains(s.CABundlePEM, "-----BEGIN CERTIFICATE-----") {
		errs = append(errs, errors.New("CABundlePEM must hold at least one PEM certificate"))
	}
	if s.HostSerial != "" && !hostSerial.MatchString(s.HostSerial) {
		errs = append(errs, fmt.Errorf("HostSerial %q is not a canonical serial number ([A-Z0-9]{6,32})", s.HostSerial))
	}
	if n := s.MaximumMessageSizeBytes; n == 0 || n > math.MaxInt64 {
		errs = append(errs, fmt.Errorf("MaximumMessageSizeBytes %d must be in [1, 2^63)", n))
	}
	if s.ReplicationConcurrency < 1 {
		errs = append(errs, fmt.Errorf("ReplicationConcurrency %d must be positive", s.ReplicationConcurrency))
	}
	if s.CacheSizeBytes < 16*MiB {
		errs = append(errs, fmt.Errorf("CacheSizeBytes %d must be at least 16 MiB", s.CacheSizeBytes))
	}
	if s.MetricsListenAddress != "" {
		if _, _, err := net.SplitHostPort(s.MetricsListenAddress); err != nil {
			errs = append(errs, fmt.Errorf("MetricsListenAddress %q: %w", s.MetricsListenAddress, err))
		}
	}
	return errors.Join(errs...)
}

// hostL2Blocks sizes the L2's blocks like a disk-backed L1 (same tiers).
func hostL2Blocks(size uint64) L1Plan {
	tier := blockTiers[len(blockTiers)-1]
	for _, t := range blockTiers {
		if size/uint64(blockCount(t, true)) >= MinimumBlockSizeBytes {
			tier = t
			break
		}
	}
	n := uint64(blockCount(tier, true))
	l := L1Plan{
		Placement:      "host-ssd",
		OldBlocks:      tier[0],
		CurrentBlocks:  tier[1],
		NewBlocks:      tier[2],
		SpareBlocks:    tier[3],
		BlockSizeBytes: size / n,
		Persistent:     true,
	}
	l.BlocksBytes = l.BlockSizeBytes * n
	l.KeyLocationMapEntries = max(minKeyLocationMapEntries, l.BlocksBytes/keyLocationMapBytesPerSlot)
	l.KeyLocationMapBytes = roundUp(l.KeyLocationMapEntries*keyLocationMapRecordBytes, 4096)
	return l
}
