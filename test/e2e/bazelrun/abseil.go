// SPDX-License-Identifier: FSL-1.1-ALv2

package bazelrun

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// Toolchain selects the C++ toolchain overlay (§10.2).
type Toolchain string

const (
	// HermeticLLVM: Linux and every cross configuration (BCR llvm 0.8.24).
	HermeticLLVM Toolchain = "hermetic-llvm"
	// Autodetect: rules_cc's autodetected MSVC (Windows) or Xcode (macOS).
	Autodetect Toolchain = "autodetect"
)

// Files under test/e2e/abseil that go on top of the unmodified Abseil
// sources: the workspace .bazelrc and the cucina_e2e package (an empty
// xcode_config and the freestanding library of build-only targets). The
// MODULE.bazel addition is rendered (ModuleOverlay); user.bazelrc and the
// cucinactl output are generated per host.
const (
	OverlayBazelrc = "abseil.bazelrc"
	// OverlayPackage is copied to <abseil>/cucina_e2e; its BUILD.overlay
	// becomes BUILD.bazel (it is not a package of the Cucina repository).
	OverlayPackage = "cucina_e2e"
)

// overlayPackageFiles are the files of OverlayPackage (source → name in the
// Abseil checkout).
var overlayPackageFiles = map[string]string{
	"BUILD.overlay":  "BUILD.bazel",
	"freestanding.c": "freestanding.c",
	"freestanding.h": "freestanding.h",
}

// ExamplePatterns is what build-only targets build (coverage "build-example":
// wasm and BPF have no OS, Abseil does not support them).
var ExamplePatterns = []string{"//" + OverlayPackage + ":freestanding"}

// Overlay is what PrepareAbseil puts on top of the unmodified Abseil sources.
type Overlay struct {
	// Dir is test/e2e/abseil on the dev Mac.
	Dir string
	// Module is appended to Abseil's MODULE.bazel (ModuleOverlay).
	Module string
}

// ModuleSegment is the generated MODULE.bazel segment of the @cucina_platforms
// module in the Cucina repository (tools/xplat; docs/cross-compilation.md).
const ModuleSegment = "tools/xplat/cucina_platforms.MODULE.bazel"

var (
	segmentDep  = regexp.MustCompile(`(?m)^bazel_dep\(name = "cucina_platforms", version = "([^"]+)"\)$`)
	segmentPath = `path = "bazel/platforms"`
)

// ModuleOverlay renders the MODULE.bazel addition for a lane. Every lane
// depends on the @cucina_platforms module (bazel/platforms, at platformsDir
// on the host) for the platform labels `cucinactl bazelrc` emits.
// hermetic-llvm lanes and every cross configuration also get the pinned
// `llvm` module, apple_support (for the empty xcode_config) and the rest of
// the generated segment: the exec-side Apple SDK (R-XPLAT-8) and the
// hermetic-llvm toolchains of the supported (exec, target) pairs — never
// @llvm//toolchain:all. The autodetect lanes (MSVC, Xcode) register nothing.
func ModuleOverlay(tc Toolchain, segment []byte, platformsDir, llvmVersion string) (string, error) {
	m := segmentDep.FindSubmatch(segment)
	if m == nil || !strings.Contains(string(segment), segmentPath) {
		return "", fmt.Errorf("%s: unexpected format (no cucina_platforms bazel_dep or local_path_override)", ModuleSegment)
	}
	path := fmt.Sprintf("path = %q", platformsDir)
	var b strings.Builder
	b.WriteString("\n# --- Cucina e2e overlay (test/e2e/bazelrun.ModuleOverlay; PROMPT §10.2, docs/cross-compilation.md) ---\n")
	switch tc {
	case HermeticLLVM:
		if llvmVersion == "" {
			return "", fmt.Errorf("hermetic-llvm lane without an llvm module version")
		}
		fmt.Fprintf(&b, "bazel_dep(name = \"llvm\", version = %q)\n", llvmVersion)
		b.WriteString("bazel_dep(name = \"apple_support\", version = \"2.9.1\")\n\n")
		b.WriteString(strings.Replace(string(segment), segmentPath, path, 1))
	case Autodetect:
		fmt.Fprintf(&b, "bazel_dep(name = \"cucina_platforms\", version = %q)\nlocal_path_override(\n    module_name = \"cucina_platforms\",\n    %s,\n)\n", m[1], path)
	default:
		return "", fmt.Errorf("unknown toolchain %q", tc)
	}
	return b.String(), nil
}

// AbseilPin is the build under test.
type AbseilPin struct {
	Repo   string // default https://github.com/abseil/abseil-cpp.git
	Tag    string // 20260817.0
	Commit string // the tag's commit, verified after cloning
}

func (p AbseilPin) repo() string {
	if p.Repo != "" {
		return p.Repo
	}
	return "https://github.com/abseil/abseil-cpp.git"
}

func join(h remote.Host, parts ...string) string {
	sep := "/"
	if h.OS() == remote.Windows {
		sep = `\`
	}
	return strings.Join(parts, sep)
}

// PrepareAbseil clones Abseil at the pinned tag into ws (once), verifies the
// commit, checks that no tracked file other than MODULE.bazel(.lock) differs
// from the tag (C++/BUILD sources unmodified), and applies the overlay:
// MODULE.bazel is reset to the tag and ov.Module appended, abseil.bazelrc
// becomes .bazelrc and the cucina_e2e package is (re)created.
func PrepareAbseil(ctx context.Context, h remote.Host, pin AbseilPin, ws string, ov Overlay, user string) error {
	if pin.Tag == "" || pin.Commit == "" {
		return fmt.Errorf("abseil pin needs tag and commit")
	}
	if ov.Module == "" {
		return fmt.Errorf("abseil overlay without a MODULE.bazel addition")
	}
	dir := join(h, h.WorkDir(), "abseil-overlay")
	uploads := map[string]string{OverlayBazelrc: join(h, dir, OverlayBazelrc)}
	for src, dst := range overlayPackageFiles {
		uploads[filepath.Join(OverlayPackage, src)] = join(h, dir, OverlayPackage, dst)
	}
	for src, dst := range uploads {
		if err := h.Put(ctx, filepath.Join(ov.Dir, src), dst); err != nil {
			return fmt.Errorf("upload overlay %s: %w", src, err)
		}
	}
	module := base64.StdEncoding.EncodeToString([]byte(ov.Module))
	var script string
	if h.OS() == remote.Windows {
		script = prepareAbseilPS(pin, ws, dir, module)
	} else {
		script = prepareAbseilSh(pin, ws, dir, module)
	}
	res, err := h.Run(ctx, script, remote.Opts{User: user})
	if err != nil {
		return err
	}
	return res.Err()
}

func prepareAbseilSh(pin AbseilPin, ws, dir, module string) string {
	return fmt.Sprintf(`set -eu
WS=%s
if [ ! -d "$WS/.git" ]; then git clone --quiet --depth 1 --branch %s %s "$WS"; fi
cd "$WS"
got=$(git rev-parse HEAD)
[ "$got" = %s ] || { echo "abseil commit mismatch: $got" >&2; exit 3; }
git checkout --quiet -- MODULE.bazel
changed=$(git diff --name-only | grep -v -x -e MODULE.bazel -e MODULE.bazel.lock || true)
[ -z "$changed" ] || { echo "abseil sources modified: $changed" >&2; exit 4; }
printf '%%s' %s | base64 -d >> MODULE.bazel
cp %s .bazelrc
rm -rf %s && cp -R %s %s
echo prepared "$got"
`, shq(ws), shq(pin.Tag), shq(pin.repo()), shq(pin.Commit), shq(module), shq(dir+"/"+OverlayBazelrc),
		OverlayPackage, shq(dir+"/"+OverlayPackage), OverlayPackage)
}

func prepareAbseilPS(pin AbseilPin, ws, dir, module string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Continue'
$ws = %s
if (-not (Test-Path (Join-Path $ws '.git'))) { git clone --quiet --depth 1 --branch %s %s $ws; if ($LASTEXITCODE) { exit $LASTEXITCODE } }
Set-Location $ws
git config core.autocrlf false
$got = (git rev-parse HEAD).Trim()
if ($got -ne %s) { [Console]::Error.WriteLine("abseil commit mismatch: $got"); exit 3 }
git checkout --quiet -- MODULE.bazel
$changed = git diff --name-only | Where-Object { $_ -ne 'MODULE.bazel' -and $_ -ne 'MODULE.bazel.lock' }
if ($changed) { [Console]::Error.WriteLine("abseil sources modified: $changed"); exit 4 }
[IO.File]::AppendAllText((Join-Path $ws 'MODULE.bazel'), [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String(%s)))
Copy-Item -Force %s .bazelrc
Remove-Item -Recurse -Force -ErrorAction SilentlyContinue %s
Copy-Item -Recurse -Force %s %s
"prepared $got"
`, psq(ws), psq(pin.Tag), psq(pin.repo()), psq(pin.Commit), psq(module), psq(dir+`\`+OverlayBazelrc),
		psq(OverlayPackage), psq(dir+`\`+OverlayPackage), psq(OverlayPackage))
}

// UserRC is the per-host user.bazelrc the overlay's .bazelrc try-imports:
// machine-local settings that must not live in a shared file. It applies to
// every invocation on the host (native lanes and cross configurations), so
// it selects no configuration.
type UserRC struct {
	OS string // linux | windows | darwin
	// RepositoryCache lives outside the output base so `bazel clean
	// --expunge` keeps it (§10.2).
	RepositoryCache string
	// Windows pins (rules_cc autodetected MSVC; identical on client and
	// workers): BAZEL_VC, BAZEL_VC_FULL_VERSION, BAZEL_WINSDK_FULL_VERSION.
	VC, VCFullVersion, WinSDKFullVersion string
	// OutputUserRoot (Windows: C:/b, short paths).
	OutputUserRoot string
}

// RCQuote quotes a token for Bazel's rc-file tokenizer, which treats a
// backslash outside single quotes as an escape (Windows paths) and splits on
// whitespace: single quotes keep everything literal.
func RCQuote(s string) string {
	switch {
	case !strings.ContainsAny(s, " \t\"'\\#"):
		return s
	case !strings.Contains(s, "'"):
		return "'" + s + "'"
	default:
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
}

// Render returns the file content.
func (u UserRC) Render() string {
	var b strings.Builder
	b.WriteString("# Generated by the Cucina e2e harness (machine-local; never committed).\n")
	if u.OS == remote.Windows {
		root := u.OutputUserRoot
		if root == "" {
			root = "C:/b"
		}
		fmt.Fprintf(&b, "startup --output_user_root=%s\nstartup --windows_enable_symlinks\n", root)
		if u.VC != "" {
			fmt.Fprintf(&b, "common %s\n", RCQuote("--repo_env=BAZEL_VC="+u.VC))
		}
		if u.VCFullVersion != "" {
			fmt.Fprintf(&b, "common --repo_env=BAZEL_VC_FULL_VERSION=%s\n", u.VCFullVersion)
		}
		if u.WinSDKFullVersion != "" {
			fmt.Fprintf(&b, "common --repo_env=BAZEL_WINSDK_FULL_VERSION=%s\n", u.WinSDKFullVersion)
		}
	} else if u.OutputUserRoot != "" {
		fmt.Fprintf(&b, "startup %s\n", RCQuote("--output_user_root="+u.OutputUserRoot))
	}
	if u.RepositoryCache != "" {
		fmt.Fprintf(&b, "common %s\n", RCQuote("--repository_cache="+u.RepositoryCache))
	}
	return b.String()
}

// WriteRCFiles writes user.bazelrc and cucina.bazelrc (the verbatim output of
// `cucinactl bazelrc …`, endpoint + credential helper + platforms + §10.2
// flags) into the workspace on the host. Remote invocations pass
// --bazelrc=<ws>/cucina.bazelrc; the local baseline does not.
func WriteRCFiles(ctx context.Context, h remote.Host, ws string, user UserRC, cucinaRC string, scratch string) error {
	for name, content := range map[string]string{"user.bazelrc": user.Render(), "cucina.bazelrc": cucinaRC} {
		local := filepath.Join(scratch, h.Name()+"-"+name)
		if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
			return err
		}
		if err := h.Put(ctx, local, join(h, ws, name)); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// DetectWindowsPins reads the Visual Studio Build Tools and Windows SDK
// versions on a Windows host (vswhere + the MSVC/SDK directories).
func DetectWindowsPins(ctx context.Context, h remote.Host) (vc, vcFull, sdkFull string, err error) {
	res, err := h.Run(ctx, `$ErrorActionPreference = 'Stop'
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
$vs = (& $vswhere -products * -latest -property installationPath | Select-Object -First 1).Trim()
$vc = Join-Path $vs 'VC'
$msvc = (Get-ChildItem (Join-Path $vc 'Tools\MSVC') | Sort-Object Name | Select-Object -Last 1).Name
$sdk = (Get-ChildItem (Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\Include') | Where-Object Name -like '10.*' | Sort-Object Name | Select-Object -Last 1).Name
"$vc|$msvc|$sdk"
`, remote.Opts{})
	if err != nil {
		return "", "", "", err
	}
	if err := res.Err(); err != nil {
		return "", "", "", err
	}
	f := strings.Split(strings.TrimSpace(string(res.Stdout)), "|")
	if len(f) != 3 || f[0] == "" || f[1] == "" || f[2] == "" {
		return "", "", "", fmt.Errorf("unexpected pin output %q", res.Stdout)
	}
	return f[0], f[1], f[2], nil
}

// AbseilTargets are the build/test patterns of §10.2.
var AbseilTargets = []string{"//absl/..."}
