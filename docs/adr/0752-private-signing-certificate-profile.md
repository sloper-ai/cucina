<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# ADR 0752: Private signing certificate: codeSigning EKU, keychain approval and CI keychains

* Status: accepted (2026-10-02, agent `pkg`)
* Requirements: R-MAC-9

## Context

Apple Business's recipe creates a self-signed leaf (RSA-4096/SHA-256, `CA:false`, `digitalSignature`, 1 year) for
`productbuild --sign`. Cucina also signs its Mach-O binaries (`codesign --timestamp --options runtime`) with the same
identity, and codesign's identity lookup uses the code-signing policy. On the dev Mac, the first `codesign` with the new
login-keychain identity blocked on a SecurityAgent dialog (the key's partition list does not admit Apple's tools until a
user approves once), as Apple's recipe warns for productbuild.

## Decision

* Add `extendedKeyUsage = critical, codeSigning` to Apple's recipe (still `CA:false`, `digitalSignature`, RSA-4096,
  SHA-256, random serial, 365 days). The certificate is trusted on hosts as a root (`com.apple.security.root`), so the
  same identity verifies for packages and code.
* Interactive Macs: the first use of the identity needs **Always Allow** once per tool (or
  `security set-key-partition-list -S apple-tool:,apple:` with the keychain password).
* Unattended signing (CI, test VMs): `make-signing-cert.sh --p12-out` produces an encrypted PKCS#12 for a secret store,
  `ci-keychain.sh` imports it into a throwaway keychain with a known random password and sets the partition list, and
  `sign.sh --keychain` signs prompt-free.

## Consequences

The test identity "Cucina Host Package Signing (test)" exists in the dev Mac's login keychain but cannot be used by an
agent until the user approves it once; T14 signs inside the throwaway VM with a PKCS#12 identity instead (same scripts).
If a future macOS rejected the EKU for package signatures, dropping it (Apple's exact recipe) and signing binaries with a
second identity would be the fallback.
