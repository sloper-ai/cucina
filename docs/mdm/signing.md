<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# Signing the Cucina host package (R-MAC-9)

Apple's only requirement for packages delivered by MDM is *"a package needs to be signed with a signature verifiable by
the device"* (Apple Platform Deployment). Cucina therefore signs with a **private certificate by default**, delivered to
the hosts by MDM as a trusted certificate before the package. A Developer ID + notarization path is scripted but **off**.

## 1. The private signing identity

Apple Business documents the recipe ("Create a package installer for an application"): a self-signed leaf certificate,
RSA-4096, SHA-256, `basicConstraints = critical, CA:false`, `keyUsage = critical, digitalSignature`, a random serial and
one year of validity, imported into a keychain for `productbuild --sign`.

`macos/pkg/scripts/make-signing-cert.sh` follows it with one addition, **`extendedKeyUsage = critical, codeSigning`**,
so the same identity also signs the Mach-O binaries with `codesign` (ADR 0752). The private key is generated in a private
temporary directory, imported **non-extractable**, and deleted; it never touches the repository or the unencrypted dev
volume. Only the public certificate is written out (PEM + DER) for the trust profile.

```sh
make -C macos/pkg cert CERT_NAME="Cucina Host Package Signing"          # login keychain (local signing)
macos/pkg/scripts/make-signing-cert.sh --name "Cucina Host Package Signing" \
  --p12-out ~/.config/cucina/pkg-signing/cucina-signing.p12             # PKCS#12 for a CI secret store instead
macos/pkg/scripts/make-signing-cert.sh --dry-run                        # print the certificate profile only
```

**Keychain access.** The key's access list names `codesign`, `productbuild`, `productsign` and `pkgbuild`, but macOS still
asks once per tool for the login password the first time the key is used (Apple's recipe warns about it: answer
**Always Allow**). On a machine where nobody can answer the dialog, either set the partition list once
(`security set-key-partition-list -S apple-tool:,apple: -s -D "<name>" ~/Library/Keychains/login.keychain-db`, asks for
the keychain password) or use a dedicated keychain:

```sh
# CI (ephemeral runner): identity from the secret store into a throwaway keychain, prompt-free
eval "$(macos/pkg/scripts/ci-keychain.sh create --p12-base64-env CUCINA_SIGNING_P12 --pass-env CUCINA_SIGNING_P12_PASS)"
make -C macos/pkg sign IDENTITY="$IDENTITY_SHA1" SIGN_KEYCHAIN="$KEYCHAIN"
macos/pkg/scripts/ci-keychain.sh delete --keychain "$KEYCHAIN"
```

## 2. What gets signed

`macos/pkg/scripts/sign.sh` (or `make -C macos/pkg sign IDENTITY=…`) rebuilds the package from the same inputs as the
unsigned build and:

* signs `cucina-hostd` and `bb_storage` with `codesign --force --timestamp --options runtime --identifier
  ai.sloper.cucina.{hostd,bb_storage}` (bb_storage's pinned upstream SHA-256 is verified before it is re-signed);
* **never re-signs `tart.app`**: re-signing would drop its restricted `com.apple.vm.networking` entitlement, which only
  Cirrus Labs' provisioning profile allows. The build verifies Tart's Team ID (`9M2P8L4D89`), CDHash and entitlement
  before and after signing;
* signs the product archive with `productbuild --sign … --timestamp`;
* verifies the result with `scripts/check-pkg.sh --signed --cert-sha1 <SHA-1>` (signature, hardened runtime,
  identifiers, signer of every binary, tart.app intact).

`pkgutil --check-signature` shows the signer; on a Mac that trusts the certificate the status is *signed by a
certificate trusted by macOS*.

## 3. Delivering trust

Profile `01-cucina-trust` (`com.apple.security.root`) installs the certificate as a trusted root in the System keychain.
MDM must deliver it **before** the package; with ADE do it while Setup Assistant awaits configuration
([setup guide §4](../macos/mac-mini-setup.md#4-what-mdm-pushes-in-order)). Apple Business's built-in **Certificate**
configuration does the same. Apple Business rejects privately signed packages on devices that lack such a trust
configuration.

## 4. Rotation

The certificate is valid for one year. Rotate well before expiry (calendar reminder at 11 months), with old and new
trusted in parallel:

1. Create the next identity with a new, unique name: `make -C macos/pkg cert CERT_NAME="Cucina Host Package Signing 2027"`.
2. Render profile 01 with both certificates (`render.sh --signer-cert old.pem --signer-cert new.pem`) and push it.
3. When every host reports the new profile, sign the next package version with the new identity and publish it
   (packages are immutable per version: re-signing means a new version and URL).
4. Once no host needs the old package any more, render profile 01 with only the new certificate and push it; delete the
   old identity from the keychain / secret store.

Timestamps (`--timestamp`) keep a signature verifiable after the certificate expires, but trust ends when the old
certificate leaves profile 01, so step 3 must complete before step 4.

## 5. Blast radius

**Anyone holding the private key can build packages that every host installs silently as root.** Keep it non-extractable
in the keychain of one build machine, or in the CI secret store (never in the repository, on the unencrypted dev volume,
or in a profile). If it may have leaked: remove the certificate from profile 01 (hosts stop trusting it at once), create a
new identity, re-sign and publish a new package version, and push the new trust profile.

## 6. Consequences of a private certificate

* **No notarization.** Not needed for MDM installs: files installed by MDM are not quarantined, so Gatekeeper never
  assesses them.
* **No Team ID.** Managed Login Items rules match the launchd **`Label`** (`ai.sloper.cucina.hostd`) or `LabelPrefix`, not
  `TeamIdentifier`; any PPPC (TCC) rule must pin the certificate:
  `identifier "ai.sloper.cucina.hostd" and anchor = H"<certificate SHA-1>"`. Cucina needs no TCC grants
  (Virtualization.framework requires none).
* **Hardened runtime** is on; library validation then only admits system libraries, which is fine for the statically
  linked Go binaries (hostd links only system frameworks).
* **Manual installs** outside MDM need the certificate trusted first (or `installer -allowUntrusted`, which Cucina's tests
  never use) and an administrator's approval.

## 7. Optional Developer ID path (off by default)

Needed for Microsoft Intune line-of-business apps, recommended by Fleet, and useful for manual installs and any future
system extension. Only runs when explicitly requested:

```sh
make -C macos/pkg sign SIGNING=developer-id \
  DEVID_APP="Developer ID Application: <Org> (<TEAMID>)" \
  DEVID_INSTALLER="Developer ID Installer: <Org> (<TEAMID>)" \
  NOTARY_KEY=~/.config/cucina/AuthKey_<KEYID>.p8 NOTARY_KEY_ID=<KEYID> NOTARY_ISSUER=<issuer-uuid>
```

* Developer ID Application for the binaries (hardened runtime + secure timestamp), Developer ID Installer for the package.
* `xcrun notarytool submit --wait` with an App Store Connect API key, then `xcrun stapler staple`.
* Verification: `pkgutil --check-signature` and `spctl --assess --type install` (must report *Notarized Developer ID*).
* The scripts refuse to use a Developer ID identity unless `SIGNING=developer-id` is set, and refuse to run this path
  without an Installer identity and notarization credentials.
