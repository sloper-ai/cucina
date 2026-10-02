<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: rotate the CA and credentials

**Use when** the Cucina CA is within six months of expiry (or compromised), a signing key must be rotated, or a long-lived credential (a service key, the break-glass key, an enrollment token) has to be replaced.
**Severity:** Planned (a compromised key is a Page: see [Revocation](revocation.md)). **Time:** hours of work spread over days for the CA, because the fleet has to catch up between phases.

## What rotates by itself, and what does not

| Credential | Lifetime | Rotation |
| --- | --- | --- |
| EC2 worker and macOS VM certificates | 24 h and 12 h | Nothing to do: a fresh one at every boot or VM start |
| Mac host certificates | 7 days, renewed at two thirds | Nothing to do: hostd renews over mTLS |
| Server certificates (frontend, STS, worker listeners, scheduler, controller: the chart's generated TLS groups) | 90 days (`tls.serverCertificateDuration`), renewed at two thirds | Nothing to do with the chart-generated CA: the controller's rotator renews them and Buildbarn reloads them (`tls.refreshInterval`). With cert-manager or your own Secret, rotate there |
| The controller's client certificate (to the scheduler's BuildQueueState API) | 30 days (`pki.controllerClientCertTTL`) | Nothing to do: renewed by the same rotator |
| **The Cucina CA** | ten years | **This runbook, section 1** |
| **JWT signing keys** | until rotated | **Section 2** |
| Site enrollment tokens | their expiry | Section 3 |
| Service-account keys, the break-glass key | until revoked | Section 3 |
| macOS package-signing certificate | one year | [Rotate the package-signing certificate](rotate-pkg-signing-cert.md) |

The metric `cucina_cert_expiry_seconds{role}` is the time to the earliest expiry per role. The controller exports `ca`, `server` and `controller`; the roles `host` and `worker` are reserved for hostd and the worker agent and are **not exported yet** (planned), so watch Mac host renewal with `cucinactl hosts list`.

The chart's alerts (`monitoring.prometheusRules`) fire for every role below `thresholds.certificateExpiry` (default 168 h, a warning) and below one day (critical). That is the "renewal failed" signal for `server` and `controller`, but it is far too late for the CA: add your own rule that fires for `role="ca"` below 180 days, which is when to start a rotation.

## 1. Rotate the Cucina CA (two-root bundle)

The CA is rotated in three phases: distribute trust in both roots before using the new signer, then retire the old root only after its leaves are gone. The phases are implemented in `internal/pki` (`RotateCA`) and described with their waiting times in
[`docs/security.md`](../security.md#ca-rotation-two-root-bundle--runbook-inputs). A CA managed by cert-manager is rotated with cert-manager instead (introduce the new root into the bundle, then follow the same waiting, restart and verification steps).

> **Planned: no operator trigger yet.** The phase functions exist, but no controller subcommand, chart hook or `cucinactl` command calls them. Until one does, this section is the specification of each phase (what it changes in the `cucina-ca` Secret, what to wait for, how to verify), not a procedure you can run end to end. Starting a rotation by hand-editing the Secret is not supported.

### Before you start

```sh
umask 077
mkdir -p -m 700 "$HOME/.config/cucina"
PKI_DIR=$(mktemp -d "$HOME/.config/cucina/pki-check.XXXXXX")
kubectl -n cucina get secret cucina-ca -o jsonpath='{.data.ca\.crt}' | base64 -d > "$PKI_DIR/bundle.pem"
awk -v dir="$PKI_DIR" '/BEGIN CERT/{n++} n {print > (dir "/ca-" n ".pem")}' "$PKI_DIR/bundle.pem"
for f in "$PKI_DIR"/ca-[0-9]*.pem; do openssl x509 -in "$f" -noout -subject -enddate -fingerprint -sha256; done
```

One certificate before rotation, two after introduce. After introduce, repeat the fetch/split into a fresh `PKI_DIR` and record the fingerprints: the appended `ca-2.pem` is CA2. Keep these files outside the checkout. Issuer names alone do not identify a root (both CAs can have the same subject).

The CA Secret holds `ca.crt` (the trust bundle), `ca.key` (the active issuing key) and, between the first two phases, `next.key`. Make sure nothing else is changing (no image rollout, no chart upgrade) and that every Mac host is Online (`cucinactl hosts list`), because hosts receive the new bundle when they renew.

### Phase 1: introduce

CA2 is generated and added to the bundle (`ca.crt` = CA1 + CA2, the staged key in `next.key`); CA1 keeps signing. **Before the next phase every verifier must trust CA2.** How each one gets it:

| Verifier | How it learns the new bundle | Check |
| --- | --- | --- |
| Frontend and scheduler (Buildbarn reads the CA bundle once, at start-up; the chart's checksum annotation does not cover it) | **You restart them** after the bundle changes: the scheduler is a single replica (`Recreate`), so expect a short gap in which Bazel retries in-flight Execute streams | `kubectl -n cucina rollout restart deploy/cucina-frontend deploy/cucina-scheduler`, then `kubectl -n cucina rollout status deploy/cucina-frontend deploy/cucina-scheduler` |
| Controller and STS replicas | Re-read the Secret every minute | wait two minutes |
| Server certificates | The rotator copies the new bundle into each managed server Secret within about ten minutes; the leaf certificates themselves are **not** re-issued yet | `for g in frontend sts frontend-workers scheduler controller; do printf '%s ' $g; kubectl -n cucina get secret cucina-tls-$g -o jsonpath='{.data.ca\.crt}' \| base64 -d \| grep -c 'BEGIN CERTIFICATE'; done` prints 2 for each |
| EC2 workers | At their next boot; the whole fleet turns over within the 12-hour maximum uptime | `cucinactl workers list` shows no worker older than 12 h |
| Mac hosts | In every enrollment and renewal response (within about 4.7 days). For hosts not yet enrolled, ship **both** CA certificates in the MDM trust and preferences profiles first | `cucinactl hosts list` shows renewed certificate expiries |

Wait for the longest of these (about five days unless you make hosts renew earlier).

### Phase 2: activate

CA2 becomes the signer (`ca.key` = the staged key, `next.key` is removed); both roots stay trusted. The rotator re-issues every server and controller certificate within about ten minutes, and Buildbarn reloads them without a restart. Verify:

```sh
kubectl -n cucina get secret cucina-controller-client -o jsonpath='{.data.tls\.crt}' | base64 -d > "$PKI_DIR/controller-client.pem"
openssl verify -CAfile "$PKI_DIR/ca-2.pem" "$PKI_DIR/controller-client.pem"   # verify against CA2, not just its subject name
cucinactl status                     # components healthy
helm test cucina -n cucina           # uses the break-glass key unless hooks.test.credentialSecret is set (see section 3)
```

New worker and host certificates are now issued by CA2. Launch a worker and confirm it registers ([worker won't register](worker-wont-register.md) if it does not).

### Phase 3: retire

Only when no certificate signed by CA1 is still in use: at least the host certificate lifetime (seven days) plus margin after activation. The bundle becomes CA2 only. **Restart the Buildbarn pods again** (`kubectl -n cucina rollout restart deploy/cucina-frontend deploy/cucina-scheduler`) so that they stop trusting CA1: they read the bundle only at start. Then remove CA1 from your MDM trust profiles
([setup guide](../macos/mac-mini-setup.md), Day 2).

There is no "abort" phase: stopping after introduce or after activate is safe, because both roots stay trusted until you retire.

### If the CA key is compromised

A compromised CA key cannot be rotated gracefully. Run all three phases in quick succession to put a new CA in place (the restarts of the Buildbarn pods included), then re-enroll the fleet: EC2 workers re-enroll at their next launch; each Mac host needs `cucinactl hosts remove <serial>`
and a new approval, because its old certificate can no longer authenticate a renewal. Treat it as an incident ([Revocation](revocation.md)). Like the phases themselves, this waits for the operator trigger (planned, above).

## 2. Rotate the JWT signing keys

Tokens are signed with an ES256 key whose public half is in the JWKS that the frontends and scheduler load. The controller's key rotator does the sequence for you: publish the new key next to the old, wait at least ten minutes **and** until it has verified that every frontend and the scheduler loaded the new key,
start signing with it, keep the old key published for the maximum token lifetime plus margin, then remove it. Builds continue throughout: tokens signed by either key validate.

* **Planned rotation.** Publish a successor key from inside the controller Pod (it prints the new key id; any replica will do, because the state is in Kubernetes objects), then watch the leader. It logs `signing key rotation started`, `signing key promotion waiting` (with the reason: lead time, or a frontend that has not loaded the key yet) and `signing key promoted`; the old key is removed from the JWKS about 17 minutes after that.

  ```sh
  kubectl -n cucina exec deploy/cucina-controller -- /usr/local/bin/cucina-controller keys rotate
  kubectl -n cucina logs -l app.kubernetes.io/component=controller,cucina.sloper.ai/leader=true -f | grep -i 'signing key'
  ```

* **Compromised signing key.** Remove it from the JWKS at once, activate a fresh key and restart the frontends and the scheduler (Buildbarn caches validated tokens and does not drop them when the JWKS changes); `--kid '*'` removes every key. Expect one failed request per client before it renews. See [Revocation](revocation.md).

  ```sh
  kubectl -n cucina exec deploy/cucina-controller -- /usr/local/bin/cucina-controller keys compromise --kid <key id> --reason "<why>"
  ```

* Never mount the JWKS or the deny-list with `subPath` (the files would never update).

## 3. Other credentials

* **Site enrollment tokens.** Create a new token, update the MDM preferences profile, then revoke the old one. Hosts that are already enrolled are unaffected.

  ```sh
  cucinactl hosts enroll-token create --site <site> --ttl 168h --max-hosts 10 --description "rotation $(date +%Y-%m)"
  cucinactl hosts enroll-token list
  cucinactl hosts enroll-token revoke <old token id>
  ```

* **Service-account keys.** Create the new key and deploy it to the system that uses it, then revoke the old one. Prefer a TTL so that keys expire on their own.

  ```sh
  cucinactl keys create --account <account> --description "<system>" --ttl 2160h
  cucinactl keys list --account <account>
  cucinactl keys revoke <old key id> --reason rotated
  ```

* **The break-glass key.** Once OIDC administrators exist, **revoke** it: `cucinactl keys revoke <key id> --reason "OIDC admins in place"`, where the id is the `key-id` entry of the `cucina-break-glass` Secret. Setting `auth.breakGlass.enabled=false` only stops the chart from generating a key: a key that is already registered keeps working (with the built-in all-verbs policy when no break-glass TrustPolicy exists), so disabling it in the values does not end access. A revoked break-glass key stays revoked across upgrades.

  `helm test` and the controller's five-minute cache canary authenticate with the break-glass key by default. Before revoking it, give both a restricted service-account key of their own (a Secret with the key in entry `key`) and set `hooks.test.credentialSecret` in your values. Disabling the Helm test does not disable the in-process canary.
* **The registry credential for macOS images** (a read-only package credential held by the controller): create the replacement, update the Secret the chart references, then revoke the old credential at the registry. Hosts receive credentials at pull time; nothing is stored in profiles or images.
* **The service-key pepper** has no supported in-place rotation procedure. Changing it invalidates every service key, including replacements created under the old pepper. Preserve it during signing-key rotation. An emergency replacement needs a separately verified OIDC administrator path and reissuance of service keys under the new pepper; do not improvise it by deleting the signing Secret.

## Verify

After any rotation: `cucinactl status` is healthy, `helm test cucina -n cucina` passes (with a credential it can use, see the break-glass note), a new worker registers, a build through the credential helper works, and `cucina_cert_expiry_seconds` shows the expected new expiries.

## Roll back

Phases are not reversed. Until you retire the old CA, both CAs are trusted, so pausing is safe. A signing key rotation that has not promoted yet can simply be left to complete. If a promoted key misbehaves, treat it as a compromised key.

## Escalate

Attach `cucinactl diag --include-logs`, the rotation phase you were in, the certificate listings from "Before you start", and the controller log for the period.
