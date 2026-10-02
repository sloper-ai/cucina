<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: revocation

**Use when** someone's access must end now (a person leaves, a laptop is stolen), a CI workflow or a service key is compromised, a signing key or CA key may have leaked, or an enrollment token was exposed. **Severity:** Page. **Time:** minutes to act; the effect is bounded by the timings below.
The reference for tokens and keys is [`docs/security.md`](../security.md).

## What you can rely on

Clients hold a Cucina JWT that lives for **15 minutes**, and Buildbarn validates it locally. Buildbarn caches validation results per token string and does not re-check them, so revocation is done in the **authorizers**, which run on every request and read a **deny-list file**. That is what makes the urgent path fast.

| Action | New token exchanges refused | Existing tokens stop working |
| --- | --- | --- |
| Account disabled at the identity provider | when its refresh and ID tokens stop working | within 15 minutes of the last exchange |
| `cucinactl keys revoke --sub <principal>` | at once on the replica that wrote it, within about 10 s on the others | **within about 3 minutes** (kubelet sync plus the 60-second reload) |
| `cucinactl keys revoke --sid <session>` | not applicable | within about 3 minutes |
| `cucinactl keys revoke <key id>` (a service key) | at once | within about 3 minutes (the key's session is deny-listed) |
| A `TrustPolicy` edited, disabled or deleted | within seconds | within 15 minutes unless you also deny-list the principal |
| Signing key compromised | at once (a new key becomes active) | when the frontends and the scheduler have restarted |

## 1. A person, a CI workflow or a session must lose access now

1. **Identify the principal.** Principals are keyed on stable IDs, never on email: `google:<numeric sub>`, `github:<repository id>:<workflow>`, `sa:<account>` for service accounts. The STS audit log (every exchange logs subject, issuer, granted verbs and result, never token values) carries **the id, not a name or an address**, so you cannot search it for a person. Get the id from your identity provider (the user's numeric ID in the admin console; for a GitHub workflow the repository ID, `gh api repos/<owner>/<repo> --jq .id`), or have the person run `cucinactl whoami` while they still can, then confirm it in the audit lines of **every** STS replica:

   ```sh
   kubectl -n cucina logs -l app.kubernetes.io/component=sts --prefix --since=24h | grep 'sts.exchange' | grep '<principal id>'
   ```

2. **Deny-list it.**

   ```sh
   cucinactl keys revoke --sub google:<numeric sub> --reason "offboarded 2026-10-02"        # the principal, all sessions
   cucinactl keys revoke --sid <session id> --reason "stolen laptop"                         # one session
   cucinactl keys revocations                                                                # what is on the deny-list now
   ```

   The command prints when the revocation is effective (at most three minutes away).

3. **Remove the source of the access** so they cannot come back after the entry is cleaned up: disable or delete the user at the identity provider; edit or delete the `TrustPolicy` that granted it (`kubectl -n cucina get trustpolicies`; the chart values hold the source of truth); for a GitHub workflow remove the repository, ref or environment from the policy.
4. **Verify.** Within three minutes the principal's next call fails (`UNAUTHENTICATED` or `PERMISSION_DENIED`) and a new login or exchange is refused. If you have a test client with that identity, run a build or `cucinactl whoami` from it.
5. **Plan for the entry staying.** Session entries expire on their own after about 17 minutes. Principal (`--sub`) entries stay, and **no command removes them yet** (`keys revocations` only lists; removal is planned), so a revoked principal cannot be restored by an administrator today. The deny-list is capped (64 KiB): prefer one `--sub` entry for a person or workflow over many session entries, and revoke only what you mean to keep revoked.

## 2. A service-account key leaked

```sh
cucinactl keys list --account <account>
cucinactl keys revoke <key id> --reason "leaked in CI log"
cucinactl keys create --account <account> --description "<system>" --ttl 2160h      # a replacement, deployed to the system
```

Revoking is effective for new exchanges at once and for outstanding tokens within about three minutes. Find where the key leaked and remove it there as well. Prefer OIDC for anything that can use it.

## 3. The break-glass key was exposed

Revoke it (`cucinactl keys revoke <key id> --reason "exposed"`; the id is in the `key-id` entry of the `<release>-break-glass` Secret) after confirming that you have another admin route (an OIDC administrator in your `trustPolicies`). A revoked break-glass key stays revoked across upgrades. Disabling it in the chart values (`auth.breakGlass.enabled=false`) is **not** a substitute: it only stops the chart from generating a key, and a key that is already registered keeps working. `helm test` uses this key unless `hooks.test.credentialSecret` names another ([rotate the CA and credentials](rotate-ca-credentials.md#3-other-credentials)).

## 4. The JWT signing key may have leaked

Anyone with the signing key can mint a token for any principal. Treat it as an incident:

1. **Rotate it as compromised** from inside the controller Pod: the key is removed from the Secret and the JWKS at once, a fresh key becomes active, and the command then restarts the frontends and the scheduler. `--kid '*'` removes every published key, which is what you want unless you know exactly which one leaked; the key ids are the `kid` entries of the published JWKS:

   ```sh
   kubectl -n cucina get configmap cucina-jwks -o jsonpath='{.data.jwks\.json}' | jq -r '.keys[].kid'
   kubectl -n cucina exec deploy/cucina-controller -- /usr/local/bin/cucina-controller keys compromise --kid '*' --reason "leaked"
   ```

2. **Confirm the frontends and the scheduler restarted.** Buildbarn caches validated tokens and does not flush them when the JWKS changes, so tokens signed by the removed key keep working until the pods restart. The command does it for you; if it reports that it could not, do it yourself:

   ```sh
   kubectl -n cucina rollout restart deploy/cucina-frontend deploy/cucina-scheduler      # only when the command could not
   kubectl -n cucina rollout status deploy/cucina-frontend deploy/cucina-scheduler
   ```

   The scheduler restart loses its in-memory queue; clients retry and builds slow down rather than fail. Expect one failed request per client before it renews transparently.
3. **Find how it leaked.** Reading the signing-key Secret is equivalent to minting any token: audit who could read it (Kubernetes RBAC, backups, logs) and tighten it.
4. **Review what was minted**: the STS audit log shows legitimate exchanges only; tokens minted with the stolen key do not appear there. Compare Buildbarn's request logs against the audit trail for the exposure window.

## 5. A worker or host identity is compromised

* **A host certificate or the host itself:** drain and remove it (`cucinactl hosts drain <serial>`, `cucinactl hosts remove <serial>`). Removal deletes the host record, releases its token slot and **deny-lists the host's identity until the certificate it held would have expired plus an hour** (seven days and an hour at most), so the certificate is refused within about three minutes. The same serial can enroll again with a token (it comes back Pending: `cucinactl hosts approve <serial>`), but its identity stays refused, and with it the host's cache upstream, until that entry lapses (`cucinactl keys revocations` shows it). If the host key may be exposed, wipe the machine.
* **A site enrollment token that leaked:** `cucinactl hosts enroll-token revoke <id>`. Hosts that are already enrolled are unaffected. The token alone is not enough to enroll: the serial number must also be pre-registered or approved, so check `cucinactl hosts list` for Pending hosts you do not recognise, and `cucinactl hosts remove` them.
* **EC2 worker credentials** are valid for at most 24 hours and are issued only against a verified instance identity; terminate the instance and let the pool replace it.
* **The Cucina CA key leaked:** see [rotate the CA and credentials](rotate-ca-credentials.md): run all three rotation phases and re-enroll the fleet.

## Verify

`cucinactl keys revocations` lists what you added; the affected principal can no longer exchange tokens or run builds; a legitimate user can; `cucinactl status` is healthy. Record the time you acted and the time the last call from the principal was refused.

## Escalate

Attach the STS audit lines for the principal, `cucinactl keys revocations`, and the time line. Never include tokens, keys or the signing-key Secret.
