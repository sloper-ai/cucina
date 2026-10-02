<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# ADR 0752: Separate private application and installer signing identities

* Status: accepted (2026-10-02, agent `pkg`)
* Requirements: R-MAC-9

## Context

[Apple Business's installer recipe](https://support.apple.com/guide/business/axm20c32e0c6/web) uses a self-signed leaf:
RSA-4096/SHA-256, critical `CA:false` and `digitalSignature`, random serial, 365 days, **no EKU**. The installer
certificate must be trusted on the signing machine as well as on destination hosts.

T14 on macOS 27 reproduced both constraints: `codesign` does not accept the no-EKU identity; `productbuild` rejects
an identity with codeSigning EKU as application-only, even when trusted. Adding the Apple installer EKU and marker did
not make the combined identity acceptable. A trusted no-EKU installer identity successfully signed a package.

## Decision

Use **two private identities**, not one dual-purpose certificate:

* `make-signing-cert.sh --purpose installer` (default): Apple's exact certificate profile for product archives.
* `--purpose application`: the same cryptographic strength and lifetime, with critical `codeSigning` EKU for Mach-O files.
* `sign.sh --identity APP --installer-identity INSTALLER` signs hostd/bb_storage and the archive respectively. Tart is
  never re-signed. `check-pkg.sh --cert-sha1 INSTALLER --app-cert-sha1 APP` verifies the two roles independently.
* The generator imports into the login keychain or exports encrypted PKCS#12; it never changes trust automatically.
  Interactive signing requires explicit installer trust and may need one key-access approval per signing tool.
* Ephemeral CI uses `ci-keychain.sh` to import both identities into one throwaway keychain with a known random password
  and partition list. Failed setup cleans up before returning; successful setup hands cleanup responsibility to the caller.
  Installer trust is a separate explicit CI operation and is removed afterwards.
* Profile 01 can hold four signer certificates (old/new application and installer) plus the optional Cucina CA, so
  rotation preserves both trust chains until the fleet has upgraded.

## Consequences

The default remains private signing, with no Developer ID or notarization requirement. There are two certificates/keys
to rotate and protect. Application PPPC requirements pin the **application** certificate, while package trust pins the
**installer** certificate. The earlier workstation test identity has codeSigning-only EKU and is not an installer identity;
this work does not alter it or any workstation trust settings. All signing/install validation runs inside throwaway VMs.
