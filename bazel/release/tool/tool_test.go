// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// R-OPS-7 / ADR 0150: one SemVer version (no build metadata, which OCI tags reject); the macOS
// package uses its numeric core (ADR 0753); a stamped build without the release workspace
// status fails instead of shipping unversioned artifacts.
func TestVersionPolicy(t *testing.T) {
	for _, tc := range []struct {
		version, core string
		pre           bool
		ok            bool
	}{
		{"0.1.0", "0.1.0", false, true},
		{"1.20.3-rc.1", "1.20.3", true, true},
		{"0.0.0-dryrun", "0.0.0", true, true},
		{"v0.1.0", "", false, false},
		{"0.1", "", false, false},
		{"01.2.3", "", false, false},
		{"1.2.3+build.5", "", false, false},
		{"1.2.3-01", "", false, false},
	} {
		core, pre, err := ParseVersion(tc.version)
		if !tc.ok {
			require.Error(t, err, tc.version)
			continue
		}
		require.NoError(t, err, tc.version)
		require.Equal(t, tc.core, core, tc.version)
		require.Equal(t, tc.pre, pre, tc.version)
	}

	status, err := ReadStatus(strings.NewReader("BUILD_USER someone\nSTABLE_CUCINA_VERSION 0.2.0\n" +
		"STABLE_CUCINA_COMMIT abc1234\nSTABLE_CUCINA_SOURCE_DATE_EPOCH 1790000000\nSTABLE_CUCINA_DIRTY false\n"))
	require.NoError(t, err)
	bi, err := BuildInfoFromStatus(status)
	require.NoError(t, err)
	require.Equal(t, BuildInfo{Version: "0.2.0", Core: "0.2.0", Commit: "abc1234", Epoch: 1790000000,
		Repository: DefaultRepository, Stamped: true}, bi)
	require.Equal(t, "cucina-host-0-2-0", PkgBase(bi.Core))

	_, err = BuildInfoFromStatus(map[string]string{"BUILD_USER": "someone"})
	require.ErrorContains(t, err, "release/workspace-status.sh")

	// R-AUTH-8: Windows argv[0] has .exe, but the public banner uses the personality name.
	for _, name := range []string{"cucinactl", "cucina-credential-helper", "cucinactl.exe", "cucina-credential-helper.exe"} {
		banner := strings.TrimSuffix(name, ".exe") + " 0.2.0"
		require.NoError(t, CheckCLIVersion(name, "0.2.0", banner))
		require.Error(t, CheckCLIVersion(name, "0.1.0", banner))
	}
}

// R-AUTH-8: Bazel runs the credential helper by path without arguments, so the archives ship
// `cucina-credential-helper` next to `cucinactl` — a hard link in tar.gz (macOS/Linux), a copy
// in zip (Windows) — and the archives are reproducible.
func TestCLIArchiveShipsCredentialHelper(t *testing.T) {
	exe := []byte("\x7fELF fake cucinactl 0.1.0")
	docs := map[string][]byte{"LICENSE.md": []byte("FSL"), "THIRD_PARTY_NOTICES.md": []byte("notices")}
	for _, tc := range []struct {
		name  string
		write func(*bytes.Buffer, ArchiveSpec) error
		exe   string
	}{
		{"x.tar.gz", func(b *bytes.Buffer, s ArchiveSpec) error { return WriteTarGz(b, s, exe, docs) }, "cucinactl"},
		{"x.zip", func(b *bytes.Buffer, s ArchiveSpec) error { return WriteZip(b, s, exe, docs) }, "cucinactl.exe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helper := strings.Replace(tc.exe, "cucinactl", "cucina-credential-helper", 1)
			spec := ArchiveSpec{Top: "cucinactl-0.1.0-x", Exe: tc.exe, Links: []string{helper}}
			var a, b bytes.Buffer
			require.NoError(t, tc.write(&a, spec))
			require.NoError(t, tc.write(&b, spec))
			require.Equal(t, a.Bytes(), b.Bytes(), "archives must be reproducible")

			entries, err := ReadArchive(tc.name, a.Bytes())
			require.NoError(t, err)
			byName := map[string]ArchiveEntry{}
			for _, e := range entries {
				byName[e.Name] = e
			}
			main, alias := byName[spec.Top+"/"+tc.exe], byName[spec.Top+"/"+helper]
			require.Equal(t, exe, main.Data)
			require.Equal(t, os.FileMode(0o755), main.Mode)
			if strings.HasSuffix(tc.name, ".zip") {
				require.Equal(t, exe, alias.Data)
			} else {
				require.Equal(t, spec.Top+"/"+tc.exe, alias.HardLink)
			}
			require.Equal(t, []byte("notices"), byName[spec.Top+"/THIRD_PARTY_NOTICES.md"].Data)
			require.Equal(t, os.FileMode(0o644), byName[spec.Top+"/LICENSE.md"].Mode)
		})
	}
}

// R-CLI-1 / ADR 0103 regression: Windows archives must not require an unbundled runtime DLL.
// debug/pe.ImportedLibraries is unimplemented; checking it silently accepted every dependency.
func TestWindowsRequiresOnlySystemDLLs(t *testing.T) {
	for _, tc := range []struct {
		dll     string
		ordinal bool
		allowed bool
	}{
		{"kernel32.dll", false, true},
		{"kernel32.dll", true, true},
		{"combase.dll", false, true},
		{"badextra.dll", false, false},
		{"badextra.dll", true, false},
	} {
		// Minimal in-memory PE import-table fixture. No executable code, compiler or filesystem.
		dos := make([]byte, 64)
		copy(dos, "MZ")
		binary.LittleEndian.PutUint32(dos[60:], 64)
		var b bytes.Buffer
		b.Write(dos)
		b.WriteString("PE\x00\x00")
		optional := pe.OptionalHeader64{Magic: 0x20b, NumberOfRvaAndSizes: 16}
		optional.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_IMPORT] = pe.DataDirectory{VirtualAddress: 0x1000, Size: 40}
		require.NoError(t, binary.Write(&b, binary.LittleEndian, pe.FileHeader{
			Machine: pe.IMAGE_FILE_MACHINE_AMD64, NumberOfSections: 1,
			SizeOfOptionalHeader: uint16(binary.Size(optional)), Characteristics: pe.IMAGE_FILE_EXECUTABLE_IMAGE,
		}))
		require.NoError(t, binary.Write(&b, binary.LittleEndian, optional))
		section := pe.SectionHeader32{VirtualSize: 512, VirtualAddress: 0x1000, SizeOfRawData: 512, PointerToRawData: 512}
		copy(section.Name[:], ".idata")
		require.NoError(t, binary.Write(&b, binary.LittleEndian, section))
		image := make([]byte, 1024)
		copy(image, b.Bytes())
		idata := image[512:]
		binary.LittleEndian.PutUint32(idata, 0x1040)      // OriginalFirstThunk
		binary.LittleEndian.PutUint32(idata[12:], 0x1060) // DLL name RVA
		binary.LittleEndian.PutUint32(idata[16:], 0x1040) // FirstThunk
		thunk := uint64(0x1080)
		if tc.ordinal {
			thunk = 1<<63 | 1
		}
		binary.LittleEndian.PutUint64(idata[0x40:], thunk)
		copy(idata[0x60:], tc.dll)
		copy(idata[0x82:], "ExitProcess") // hint (2 bytes) + import name
		err := CheckBinary(image, Platform{"windows", "amd64"})
		if tc.allowed {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, tc.dll)
		}
	}
}

// R-OPS-7: the packaged chart pins the controller image it was released with (repository, tag
// and the digest of the pushed index) without editing anything else in values.yaml.
func TestSetControllerImage(t *testing.T) {
	in := "# header comment\nimages:\n  pullPolicy: IfNotPresent\n  controller:\n" +
		"    repository: ghcr.io/sloper-ai/cucina-controller\n    tag: 0.1.0 # set by the release\n" +
		"    digest: \"\"\n  sts:\n    tag: \"\"\nother: [1, 2]\n"
	digest := "sha256:" + strings.Repeat("ab", 32)
	out, err := SetControllerImage([]byte(in), "ghcr.io/fork/cucina-controller", "1.2.3-rc.1", digest)
	require.NoError(t, err)
	want := strings.NewReplacer(
		"repository: ghcr.io/sloper-ai/cucina-controller", `repository: "ghcr.io/fork/cucina-controller"`,
		"tag: 0.1.0 # set by the release", `tag: "1.2.3-rc.1" # set by the release`,
		`digest: ""`, `digest: "`+digest+`"`,
	).Replace(in)
	require.Equal(t, want, string(out))

	_, err = SetControllerImage([]byte("images:\n  controller: ghcr.io/x\n"), "r", "1.0.0", digest)
	require.Error(t, err)
}
