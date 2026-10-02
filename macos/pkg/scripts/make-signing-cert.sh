#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Creates Cucina's private package-signing identity (R-MAC-9, default signing path) following Apple Business's
# documented recipe ("Create a package installer for an application": self-signed leaf, RSA-4096/SHA-256,
# basicConstraints critical CA:false, keyUsage critical digitalSignature, random serial, 1-year validity). One
# addition to Apple's recipe: extendedKeyUsage codeSigning, so the same identity also signs the Mach-O binaries
# with codesign (ADR 0752).
#
# Two destinations (the private key never touches the repository or the unencrypted dev volume):
#   default        import into a keychain (default: the login keychain) for local signing. The key's access list
#                  names codesign/productbuild/productsign/pkgbuild; macOS still asks ONCE per tool for the login
#                  password ("Always Allow"), exactly as Apple's recipe warns, unless the partition list is set:
#                  security set-key-partition-list -S apple-tool:,apple: -s -D "<name>" login.keychain-db
#   --p12-out FILE write an encrypted PKCS#12 (FILE, 0600) and its random passphrase (FILE.pass, 0600) instead,
#                  for a CI secret store; CI imports it into a throwaway keychain with scripts/ci-keychain.sh.
# The PUBLIC certificate is always written (PEM + DER) for the trust profile (macos/profiles, 01-cucina-trust).
#
# usage: make-signing-cert.sh [--name CN] [--days N] [--keychain PATH] [--out-dir DIR] [--extractable]
#                             [--p12-out FILE] [--dry-run]
#   --name CN       certificate common name (default: "Cucina Host Package Signing"); use a new, unique name per
#                   rotation (e.g. "... 2027") so codesign identities stay unambiguous
#   --days N        validity in days (default: 365; re-sign and rotate before expiry, docs/mdm/signing.md)
#   --keychain PATH target keychain (default: ~/Library/Keychains/login.keychain-db)
#   --out-dir DIR   public certificate directory (default: ~/.config/cucina/pkg-signing, mode 0700)
#   --extractable   allow exporting the imported private key later (default: non-extractable)
#   --dry-run       generate into a temporary directory and print the certificate only; no keychain change
set -eu

. "$(dirname -- "$0")/lib.sh"

name="Cucina Host Package Signing"
days=365
keychain=$HOME/Library/Keychains/login.keychain-db
out_dir=$HOME/.config/cucina/pkg-signing
extractable=0 dry_run=0 p12_out=''
while [ $# -gt 0 ]; do
	case $1 in
	--name) name=$2 && shift 2 ;;
	--days) days=$2 && shift 2 ;;
	--keychain) keychain=$2 && shift 2 ;;
	--out-dir) out_dir=$2 && shift 2 ;;
	--extractable) extractable=1 && shift ;;
	--p12-out) p12_out=$2 && shift 2 ;;
	--dry-run) dry_run=1 && shift ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' && exit 0 ;;
	*) cucina_die "unknown argument: $1 (try --help)" ;;
	esac
done
case $days in '' | *[!0-9]*) cucina_die "--days must be a number" ;; esac
case $name in *[\"/\\=+,\;\<\>#]*) cucina_die "--name must not contain any of \" / \\ = + , ; < > #" ;; esac
cucina_need openssl security shasum

if [ "$dry_run" = 0 ] && [ -z "$p12_out" ] && security find-certificate -c "$name" "$keychain" >/dev/null 2>&1; then
	cucina_die "a certificate named '$name' already exists in $keychain (codesign needs an unambiguous name;" \
		"pick another --name for a rotation, e.g. '$name 2027')"
fi
if [ -n "$p12_out" ] && [ -e "$p12_out" ]; then cucina_die "$p12_out exists"; fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/cucina-signing.XXXXXX")
chmod 0700 "$tmp"
trap 'rm -rf "$tmp"' EXIT INT TERM

cat >"$tmp/openssl.cnf" <<EOF
[ req ]
distinguished_name = dn
prompt = no
x509_extensions = leaf
[ dn ]
CN = $name
[ leaf ]
basicConstraints = critical, CA:false
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, codeSigning
subjectKeyIdentifier = hash
EOF

serial=0x01$(openssl rand -hex 8)
openssl req -x509 -new -newkey rsa:4096 -sha256 -nodes -days "$days" -set_serial "$serial" \
	-config "$tmp/openssl.cnf" -keyout "$tmp/key.pem" -out "$tmp/cert.pem" 2>/dev/null
chmod 0600 "$tmp/key.pem"
openssl x509 -in "$tmp/cert.pem" -outform DER -out "$tmp/cert.cer"
sha1=$(shasum -a 1 "$tmp/cert.cer" | cut -d ' ' -f 1 | tr 'a-f' 'A-F')
sha256=$(shasum -a 256 "$tmp/cert.cer" | cut -d ' ' -f 1)
not_after=$(openssl x509 -in "$tmp/cert.pem" -noout -enddate | sed 's/^notAfter=//')
openssl x509 -in "$tmp/cert.pem" -noout -subject -serial -dates >&2
openssl x509 -in "$tmp/cert.pem" -noout -text | grep -E 'Public-Key|Signature Algorithm' | sort -u >&2
openssl x509 -in "$tmp/cert.pem" -noout -text | sed -n '/X509v3 extensions/,/Signature Algorithm/p' | sed '$d' >&2

if [ "$dry_run" = 1 ]; then
	printf 'dry-run: SHA-1 %s  SHA-256 %s  (nothing imported)\n' "$sha1" "$sha256"
	exit 0
fi

if [ -n "$p12_out" ]; then
	# 3DES/SHA-1 PKCS#12: the encryption macOS's `security import` accepts from both LibreSSL and OpenSSL 3.
	openssl rand -base64 33 | tr -d '\n' >"$tmp/pass"
	(umask 077 && openssl pkcs12 -export -inkey "$tmp/key.pem" -in "$tmp/cert.pem" -name "$name" \
		-keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES -macalg sha1 -passout "file:$tmp/pass" -out "$p12_out")
	(umask 077 && cp "$tmp/pass" "$p12_out.pass")
	dest="PKCS#12 $p12_out (passphrase in $p12_out.pass; move both into the CI secret store, then delete them)"
else
	# Import the certificate, then the key with access for Apple's signing tools (Apple's recipe, plus codesign).
	set -- -k "$keychain" -T /usr/bin/codesign -T /usr/bin/productbuild -T /usr/bin/productsign -T /usr/bin/pkgbuild
	[ "$extractable" = 1 ] || set -- "$@" -x
	security import "$tmp/cert.pem" -k "$keychain" >/dev/null
	security import "$tmp/key.pem" "$@" >/dev/null
	security find-identity -p codesigning "$keychain" | grep -q "$sha1" ||
		cucina_warn "the identity is not listed by 'security find-identity -p codesigning' (check the import)"
	dest="keychain $keychain (private key $([ "$extractable" = 1 ] && echo extractable || echo non-extractable))"
fi
rm -f "$tmp/key.pem"

mkdir -p "$out_dir"
chmod 0700 "$out_dir"
base=$out_dir/$(printf '%s' "$name" | tr ' ' '-' | tr -cd 'A-Za-z0-9._-')-$(printf '%s' "$sha1" | cut -c1-8)
cp "$tmp/cert.pem" "$base.pem"
cp "$tmp/cert.cer" "$base.cer"
chmod 0644 "$base.pem" "$base.cer"

cat <<EOF
created signing identity "$name"
  stored in:     $dest
  SHA-1:         $sha1   (codesign/productbuild identity; PPPC: anchor = H"$sha1")
  SHA-256:       $sha256
  expires:       $not_after   (re-sign and rotate before then: docs/mdm/signing.md)
  public cert:   $base.pem   (trust profile: macos/profiles/render.sh --signer-cert $base.pem)
EOF
