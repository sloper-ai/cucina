// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"strings"
)

// windowsSystemDLLs may be imported by the Windows binaries: anything else (libunwind,
// libc++, a MinGW runtime DLL) would be missing on a stock Windows host (ADR 0103).
var windowsSystemDLLs = map[string]bool{
	"advapi32.dll": true, "bcrypt.dll": true, "bcryptprimitives.dll": true, "crypt32.dll": true,
	"combase.dll":  true, // WinRT, Windows 8+/Server 2012+ (Microsoft Learn: roapi/RoInitialize)
	"iphlpapi.dll": true, "kernel32.dll": true, "msvcrt.dll": true, "ncrypt.dll": true,
	"ntdll.dll": true, "ole32.dll": true, "oleaut32.dll": true, "powrprof.dll": true,
	"secur32.dll": true, "shell32.dll": true, "shlwapi.dll": true, "user32.dll": true,
	"userenv.dll": true, "winmm.dll": true, "ws2_32.dll": true, "dbghelp.dll": true,
	"psapi.dll": true, "mswsock.dll": true, "netapi32.dll": true, "rpcrt4.dll": true,
}

// CheckBinary verifies that data is an executable for platform p: the object format and CPU,
// static linking on Linux (musl/pure Go: no ELF interpreter) and only system DLLs on Windows.
func CheckBinary(data []byte, p Platform) error {
	r := bytes.NewReader(data)
	switch p.OS {
	case "darwin":
		f, err := macho.NewFile(r)
		if err != nil {
			return fmt.Errorf("not a Mach-O executable: %w", err)
		}
		want := map[string]macho.Cpu{"arm64": macho.CpuArm64, "amd64": macho.CpuAmd64}[p.Arch]
		if f.Cpu != want || f.Type != macho.TypeExec {
			return fmt.Errorf("Mach-O %v type %v, want %v executable", f.Cpu, f.Type, want)
		}
	case "linux":
		f, err := elf.NewFile(r)
		if err != nil {
			return fmt.Errorf("not an ELF executable: %w", err)
		}
		want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[p.Arch]
		if f.Machine != want {
			return fmt.Errorf("ELF machine %v, want %v", f.Machine, want)
		}
		for _, prog := range f.Progs {
			if prog.Type == elf.PT_INTERP {
				return fmt.Errorf("dynamically linked (has an ELF interpreter); release binaries are static")
			}
		}
	case "windows":
		f, err := pe.NewFile(r)
		if err != nil {
			return fmt.Errorf("not a PE executable: %w", err)
		}
		if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
			return fmt.Errorf("PE machine %#x, want AMD64", f.Machine)
		}
		libs, err := windowsImports(f)
		if err != nil {
			return err
		}
		for _, l := range libs {
			l = strings.ToLower(l)
			if !windowsSystemDLLs[l] && !strings.HasPrefix(l, "api-ms-win-") {
				return fmt.Errorf("imports %s, which is not an approved system DLL", l)
			}
		}
	default:
		return fmt.Errorf("unknown OS %q", p.OS)
	}
	return nil
}

// Read import descriptor names after debug/pe has parsed the section layout. Go 1.27's
// ImportedLibraries is a no-op; ImportedSymbols omits ordinal-only imports, so neither proves
// the release's DLL dependency contract. Names in the import directory cover both forms.
func windowsImports(f *pe.File) ([]string, error) {
	h, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok {
		return nil, fmt.Errorf("windows release is not PE32+")
	}
	rvaData := func(rva uint32) ([]byte, error) {
		for _, s := range f.Sections {
			if rva >= s.VirtualAddress && uint64(rva)-uint64(s.VirtualAddress) < uint64(s.Size) {
				data, err := s.Data()
				if err != nil {
					return nil, err
				}
				return data[rva-s.VirtualAddress:], nil
			}
		}
		return nil, fmt.Errorf("PE import RVA %#x is outside its sections", rva)
	}
	dir := h.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_IMPORT]
	if dir.VirtualAddress == 0 {
		return nil, nil
	}
	data, err := rvaData(dir.VirtualAddress)
	if err != nil {
		return nil, err
	}
	if dir.Size > uint32(len(data)) {
		return nil, fmt.Errorf("truncated PE import directory")
	}
	data = data[:dir.Size]
	var libraries []string
	for len(data) >= 20 {
		descriptor := data[:20]
		data = data[20:]
		if bytes.Equal(descriptor, make([]byte, 20)) {
			return libraries, nil
		}
		name, err := rvaData(binary.LittleEndian.Uint32(descriptor[12:16]))
		if err != nil {
			return nil, err
		}
		end := bytes.IndexByte(name, 0)
		if end <= 0 {
			return nil, fmt.Errorf("missing or unterminated PE DLL name")
		}
		libraries = append(libraries, string(name[:end]))
	}
	return nil, fmt.Errorf("unterminated PE import directory")
}

// ContainsVersion reports whether the binary embeds the version string (Go -X / Rust env!
// constants are stored verbatim). It is a necessary, not a sufficient, condition; native
// execution checks the reported value where the host can run the binary.
func ContainsVersion(data []byte, version string) bool {
	return bytes.Contains(data, []byte(version))
}
