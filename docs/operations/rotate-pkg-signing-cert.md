<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: rotate the package-signing certificate

**Use when** the private certificate that signs the macOS host package and its binaries is within 60 days of expiry, or when its private key may have leaked. **Severity:** Planned; a leaked key is a Page. **Time:** an hour of work, spread over the days it takes for every Mac to receive the
new trust profile. The signing certificate is valid for one year by default, so put its expiry in a calendar.

## Background

Cucina's default signing path uses a **private certificate** rather than an Apple Developer ID: a self-signed leaf (RSA-4096 and SHA-256, a one-year life) that signs the package (`productbuild --sign`) and the Mach-O binaries (`codesign --timestamp`). Apple's only requirement for MDM-delivered packages is that the signature is verifiable by the
device, so MDM first delivers the certificate to every Mac as a **trust profile**, and only afterwards the package. The package's `tart.app` is never re-signed.

Because a Mac trusts whatever this key signs, rotation must keep **old and new certificates trusted in parallel** until every Mac has received the new one, and **the key is a crown jewel**: anyone holding it can build a package that Macs trust silently.

## Rotation (planned)

1. **Check the current expiry, and note the identity's SHA-1.** Names are matched as substrings by `security` and `codesign`, and the old name is a prefix of the new one below, so from now on refer to each identity by its SHA-1, never by name.

   ```sh
   security find-certificate -c "Cucina Host Package Signing" -p | openssl x509 -noout -subject -enddate
   security find-identity -p codesigning | grep "Cucina Host Package Signing"      # the 40-hex SHA-1 of each identity
   ```

2. **Create the new identity** in the login keychain of the signing machine, under a new name so the two can coexist. The script writes only the *public* certificate (PEM and DER) to `~/.config/cucina/pkg-signing/`; the private key never touches the repository or the shared volume. It prints the new certificate's SHA-1: note it.

   ```sh
   make -C macos/pkg cert CERT_NAME="Cucina Host Package Signing 2027"
   ```

3. **Trust both certificates.** Render the trust profile (profile 01) with the old and the new public certificate, upload it to the MDM, and let it reach the fleet (the profile templates and settings are described in the [profiles guide](../mdm/profiles.md)). The renderer also needs the Cucina CA (or a pin) so that the host agent can verify the controller, and refuses without it:

   ```sh
   macos/profiles/render.sh --config <your values file> --ca-cert <cucina-ca.pem> --signer-cert old.pem --signer-cert new.pem --only 01
   ```

   Check a host (SSH or the MDM's remote tool):

   ```sh
   sudo profiles list | grep -i trust
   security find-certificate -a -c "Cucina Host Package Signing" /Library/Keychains/System.keychain
   ```

   **Do not sign with the new key until every Mac lists both certificates.** A Mac that has only the old certificate will refuse a package signed by the new one.

4. **Build, sign and publish the next package with the new identity.**

   ```sh
   make -C macos/pkg sign IDENTITY=<SHA-1 of the new identity> VERSION=<next version>
   make -C macos/pkg check VERSION=<next version>
   make -C macos/pkg manifest VERSION=<next version>
   make -C macos/pkg pkg-publish VERSION=<next version>
   ```

   Pass the same `VERSION` to every command: it names the file in `macos/pkg/build/`. Without it `check` quietly checks nothing (it skips a package that does not exist), and `manifest` and `pkg-publish` work on the default version's package instead of this one.
   Publishing uploads a versioned, immutable package and its manifest (URL, SHA-256 and bundle identifier) and verifies them. Update the package entry in the MDM to the new version, and let it roll out.

5. **Verify on a host:** `pkgutil --pkg-info ai.sloper.cucina.host` shows the new version, the daemon runs (`sudo launchctl print system/ai.sloper.cucina.hostd`), and `cucinactl hosts list` shows the host Online with the new agent version.
6. **Retire the old certificate** only after every host runs a package signed by the new one and nothing will install the old package again: render profile 01 with only the new certificate and push it, then delete the old identity from the signing machine's keychain by its SHA-1 (`security delete-identity -Z <SHA-1 of the old identity>`; deleting by name would match the new identity too). This cannot be undone: keep the old public certificate in your records.

Packages are immutable per version, so signing with a new identity means a new package version and URL. The full background (consequences of a private certificate, the optional Developer ID path that is off by default, and notarization) is in the [signing guide](../mdm/signing.md).

## If the key leaked

1. **Stop trusting it now.** Render profile 01 without the leaked certificate and push it. Macs stop trusting new packages signed by it; software that is already installed keeps running.
2. Create a new identity (step 2 above) and put **only** the new certificate in the trust profile. Build and sign a clean package from a known-good commit and publish it.
3. Audit what Macs were told to install: the package URLs and checksums in the MDM, the published releases and manifests, and any package outside a release that carries the old signature. Re-image a host if you find something you cannot explain.
4. Delete the leaked identity everywhere and record the incident.

## Where the key lives

For local use the private key is in the signing machine's login keychain (non-extractable by default). In production keep it in your CI secret store, give signing jobs access only on protected branches or tags, and never commit it or write it to the shared development volume ([`TESTING.md`](../../TESTING.md) section 6.3).

## Roll back

Until you retire the old certificate both are trusted, so you can go back to signing with the old identity (`make -C macos/pkg sign IDENTITY=<SHA-1 of the old identity> VERSION=<version>`). After step 6 you cannot.

## Verify

* Every host lists both certificates before step 4, and only the new one after step 6.
* A freshly installed package passes without `-allowUntrusted`.
* The old certificate's expiry no longer matters.

## Escalate

Attach the output of step 1, the profile versions in the MDM, and the list of hosts that did not report the new certificate.
