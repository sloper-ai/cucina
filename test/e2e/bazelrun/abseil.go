// SPDX-License-Identifier: FSL-1.1-ALv2

package bazelrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// Overlay names the files under test/e2e/abseil that are allowed on top of
// the unmodified Abseil sources: a MODULE.bazel addition, the workspace
// .bazelrc, and (generated per host) user.bazelrc + cucina.bazelrc.
const (
	OverlayModuleLLVM = "MODULE.hermetic-llvm.bazel"
	OverlayBazelrc    = "abseil.bazelrc"
)

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
// from the tag (C++/BUILD sources unmodified), and applies the overlay for
// the toolchain. overlayDir is test/e2e/abseil on the dev Mac.
func PrepareAbseil(ctx context.Context, h remote.Host, pin AbseilPin, ws, overlayDir string, tc Toolchain, user string) error {
	if pin.Tag == "" || pin.Commit == "" {
		return fmt.Errorf("abseil pin needs tag and commit")
	}
	ov := join(h, h.WorkDir(), "abseil-overlay")
	files := []string{OverlayBazelrc}
	if tc == HermeticLLVM {
		files = append(files, OverlayModuleLLVM)
	}
	for _, f := range files {
		if err := h.Put(ctx, filepath.Join(overlayDir, f), join(h, ov, f)); err != nil {
			return fmt.Errorf("upload overlay %s: %w", f, err)
		}
	}
	var script string
	if h.OS() == remote.Windows {
		script = prepareAbseilPS(pin, ws, ov, tc)
	} else {
		script = prepareAbseilSh(pin, ws, ov, tc)
	}
	res, err := h.Run(ctx, script, remote.Opts{User: user})
	if err != nil {
		return err
	}
	return res.Err()
}

func prepareAbseilSh(pin AbseilPin, ws, ov string, tc Toolchain) string {
	module := ""
	if tc == HermeticLLVM {
		module = fmt.Sprintf("cat %s >> MODULE.bazel\n", shq(ov+"/"+OverlayModuleLLVM))
	}
	return fmt.Sprintf(`set -eu
WS=%s
if [ ! -d "$WS/.git" ]; then git clone --quiet --depth 1 --branch %s %s "$WS"; fi
cd "$WS"
got=$(git rev-parse HEAD)
[ "$got" = %s ] || { echo "abseil commit mismatch: $got" >&2; exit 3; }
git checkout --quiet -- MODULE.bazel
changed=$(git diff --name-only | grep -v -x -e MODULE.bazel -e MODULE.bazel.lock || true)
[ -z "$changed" ] || { echo "abseil sources modified: $changed" >&2; exit 4; }
%scp %s .bazelrc
echo prepared "$got"
`, shq(ws), shq(pin.Tag), shq(pin.repo()), shq(pin.Commit), module, shq(ov+"/"+OverlayBazelrc))
}

func prepareAbseilPS(pin AbseilPin, ws, ov string, tc Toolchain) string {
	module := ""
	if tc == HermeticLLVM {
		module = fmt.Sprintf("Get-Content -Raw %s | Add-Content -NoNewline MODULE.bazel\n", psq(ov+`\`+OverlayModuleLLVM))
	}
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
%sCopy-Item -Force %s .bazelrc
"prepared $got"
`, psq(ws), psq(pin.Tag), psq(pin.repo()), psq(pin.Commit), module, psq(ov+`\`+OverlayBazelrc))
}

// UserRC is the per-host user.bazelrc the overlay's .bazelrc try-imports:
// machine-local settings that must not live in a shared file.
type UserRC struct {
	OS string // linux | windows | darwin
	// RepositoryCache lives outside the output base so `bazel clean
	// --expunge` keeps it (§10.2).
	RepositoryCache string
	// PlatformsDir is injected as @cucina_platforms (Bazel 9
	// --inject_repository), the platform definitions cucinactl bazelrc
	// references.
	PlatformsDir string
	// Windows pins (rules_cc autodetected MSVC; identical on client and
	// workers): BAZEL_VC, BAZEL_VC_FULL_VERSION, BAZEL_WINSDK_FULL_VERSION.
	VC, VCFullVersion, WinSDKFullVersion string
	// OutputUserRoot (Windows: C:/b, short paths).
	OutputUserRoot string
	// Hermetic enables --config=hermetic (hermetic-llvm lanes).
	Hermetic bool
}

func rcQuote(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
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
			fmt.Fprintf(&b, "common --repo_env=%s\n", rcQuote("BAZEL_VC="+u.VC))
		}
		if u.VCFullVersion != "" {
			fmt.Fprintf(&b, "common --repo_env=BAZEL_VC_FULL_VERSION=%s\n", u.VCFullVersion)
		}
		if u.WinSDKFullVersion != "" {
			fmt.Fprintf(&b, "common --repo_env=BAZEL_WINSDK_FULL_VERSION=%s\n", u.WinSDKFullVersion)
		}
	} else if u.OutputUserRoot != "" {
		fmt.Fprintf(&b, "startup --output_user_root=%s\n", rcQuote(u.OutputUserRoot))
	}
	if u.RepositoryCache != "" {
		fmt.Fprintf(&b, "common --repository_cache=%s\n", rcQuote(u.RepositoryCache))
	}
	if u.PlatformsDir != "" {
		fmt.Fprintf(&b, "common --inject_repository=%s\n", rcQuote("cucina_platforms="+u.PlatformsDir))
	}
	if u.Hermetic {
		b.WriteString("common --config=hermetic\n")
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
