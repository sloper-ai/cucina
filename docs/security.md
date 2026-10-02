<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Cucina security

This document is the security reference for Cucina. It is organised by topic; each top-level section stands
on its own so that it can be maintained independently:

* **Workload identity** — the X.509 identities of workers, Mac hosts, VMs, the controller and the in-cluster
  servers: exact URI SAN strings and the verification rules every verifier (Buildbarn and Cucina) applies.
* **PKI** — Cucina's private CA, certificate profiles, the CA Secret layout, rotation.
* **Enrollment** — how EC2 workers and Mac hosts obtain their first certificate without baked secrets (R-SEC-3).
* **Workload identity threat model** — what a stolen worker certificate, host certificate or site token can do.
* Token sections (Cucina JWT, STS, trust policies, deny-list, revocation) — client authentication (R-AUTH).

---

## Workload identity

Workers, Mac hosts, the VMs on Mac hosts and the controller authenticate with short-lived X.509 certificates issued
by Cucina's private CA (R-SEC-2). The identity is a **SPIFFE-style URI SAN**; nothing else in the certificate
(subject, DNS names, organisation) carries identity, and verifiers must never look at anything but the URI SAN.

### URI SAN formats (normative)

Every workload certificate carries **exactly one** URI SAN and **no** DNS, IP or e-mail SANs. The trust domain is the
literal `cucina` (one CA per Cucina installation; never share the workload CA between installations).

| Role | URI SAN (exact) | Issued by | Default TTL (hard max) | EKU |
| --- | --- | --- | --- | --- |
| EC2 worker | `spiffe://cucina/worker/<pool>/<instance-id>` | `EnrollmentService.EnrollWorker`, at **every** boot | 24 h (7 d) | clientAuth |
| macOS VM worker | `spiffe://cucina/worker/<pool>/<serial>/<vm>` | `HostService.IssueVMIdentity`, only to the VM's own host | 12 h (12 h) | clientAuth |
| Mac host | `spiffe://cucina/host/<serial>` | `EnrollmentService.EnrollHost` (once), then `HostService.RenewCertificate` | 7 d (7 d), renewed at 2/3 of its lifetime | clientAuth |
| Controller (client) | `spiffe://cucina/controller` | `cucina-controller bootstrap` / leader-only rotator | 90 d (configurable), renewed at 2/3 | clientAuth |
| In-cluster server | `spiffe://cucina/server/<component>` **plus** the DNS/IP SANs of the listener | `cucina-controller bootstrap` / leader-only rotator | 90 d (configurable), renewed at 2/3 | serverAuth (+ clientAuth only where the component dials mTLS) |

Segment grammar (anything else is never issued and must be rejected by Cucina's own verifiers):

| Segment | Meaning | Grammar |
| --- | --- | --- |
| `<pool>` | `WorkerPool` name (also the `pool` worker-id label and the `cucina:pool` tag) | `[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?` |
| `<instance-id>` | EC2 instance ID (also the `node` worker-id label) | `i-[0-9a-f]{8}` or `i-[0-9a-f]{17}` |
| `<serial>` | Mac hardware serial number, canonical form: ASCII upper case, no spaces | `[A-Z0-9]{6,32}` |
| `<vm>` | Tart VM name chosen by the controller | `[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?` |
| `<component>` | in-cluster server component | `frontend`, `storage`, `scheduler`, `controller`, `sts` (`[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`) |

* The `node` worker-id label of a macOS VM worker is `<serial>/<vm>` — the same two segments as in its URI SAN, so
  the certificate and the scheduler's worker ID name the same VM.
* Worker-class identities (EC2 workers **and** macOS VMs) share the prefix `spiffe://cucina/worker/`; host identities
  use `spiffe://cucina/host/`. Rules that mean "any worker" match the prefix, never a segment count.
* The strings above are what Buildbarn sees: it exposes each URI SAN as Go's `url.URL.String()`, which is
  byte-identical to the issued string because every segment is restricted to `[A-Za-z0-9._-]`.
* Constructors and parsers live in `internal/pki` (`pki.WorkerIdentity`, `pki.VMIdentity`, `pki.HostIdentity`,
  `pki.ControllerIdentity`, `pki.ServerIdentity`, `pki.ParseIdentity`, `pki.ParseIdentityURL`, `pki.CanonicalSerial`).
  The Buildbarn expressions below are exported as Go constants there (`pki.Buildbarn*`) and a test runs certificates
  issued by the real profiles through **Buildbarn's own** verifier and JMESPath authorizers (bb-storage `pkg/x509`,
  `pkg/auth` at the pinned version) with exactly these expressions.

### Verification rules for Buildbarn (`tlsClientCertificate`, rendered by the chart)

Buildbarn (bb-storage `ClientCertificateVerifierConfiguration`, identical in both pinned bb-storage versions) verifies
the presented chain against `clientCertificateAuthorities` **with EKU clientAuth**, then evaluates the validation
expression over `{"dnsNames": […], "emailAddresses": […], "uris": […]}` of the leaf (the result must be exactly
boolean `true`), then the metadata expression. It uses go-jmespath v0.4.0: standard JMESPath functions only (no
`split`, no regex), raw string literals `'…'`, JSON literals in backticks.

1. **`clientCertificateAuthorities`** = the PEM **CA bundle** (`ca.crt` of the CA Secret, see §PKI). During a CA
   rotation it holds two roots. The field is inline PEM, read at start-up: a bundle change needs a rolling restart of
   the Buildbarn pods (checksum annotation), which is why rotation has a separate *introduce* phase.
2. **Never** use `` `true` `` as the validation expression on a listener that accepts certificates: server and
   controller certificates also chain to the same CA.
3. The metadata produced from a certificate uses the same `private` keys as the Cucina JWT's `cucina` claim object
   (`cas_read`, `cas_write`, `ac_read`, `ac_write`, `execute`: explicit instance-name lists) plus `sub` = the URI SAN,
   so one set of per-operation authorizers (`contains(authenticationMetadata.private.<verb>, instanceName)` AND the
   deny-list check on `sub`) serves JWT clients and certificate holders alike. Rendering the instance-name lists from
   `values.instanceNames` (and per pool from `WorkerPool.spec.instanceNames`) is the chart's job.

**Frontend, worker/host listener** (CAS, AC incl. AC writes of remotely executed actions; R-SEC-2):

```text
validationJmespathExpression:
  length(uris) == `1` && (starts_with(uris[0], 'spiffe://cucina/worker/') || starts_with(uris[0], 'spiffe://cucina/host/'))

metadataExtractionJmespathExpression (instance names rendered by the chart; here only "main"):
  {public: {user: uris[0]}, private: {sub: uris[0], cas_read: `["main"]`, cas_write: `["main"]`, ac_read: `["main"]`, ac_write: `["main"]`, execute: `[]`}}
```

Hosts are admitted on this listener because a host's L2 cache (`bb_storage` on the Mac) is the upstream client for
its VMs: CAS reads/writes and the VMs' AC writes reach the frontend with the **host's** certificate. A host already
controls its VMs, so allowing it AC writes does not widen what a compromised host can do. If the L2 design ever sends
VM AC writes directly upstream, drop `host` from `ac_write` (render `ac_write: \`[]\`` for the host prefix).

Per-pool tenant binding (optional, recommended when pools serve different instance names): render the lists per pool
prefix with JMESPath's `&&`/`||` value semantics, for example

```text
ac_write: (starts_with(uris[0], 'spiffe://cucina/worker/pool-a/') && `["tenant-a"]`) || (starts_with(uris[0], 'spiffe://cucina/worker/pool-b/') && `["tenant-b"]`) || `[]`
```

**Scheduler, `workerGrpcServers`** (Synchronize; only workers, never hosts — VM workers reach it through their host's
TCP relay with their **own** certificate):

```text
validationJmespathExpression:
  length(uris) == `1` && starts_with(uris[0], 'spiffe://cucina/worker/')
metadataExtractionJmespathExpression:
  {public: {user: uris[0]}, private: {sub: uris[0]}}
synchronizeAuthorizer (jmespathExpression; AND the deny-list check on authenticationMetadata.private.sub):
  starts_with(authenticationMetadata.private.sub, 'spiffe://cucina/worker/')
```

Buildbarn's synchronize authorizer only sees `{authenticationMetadata, instanceName}`; it cannot bind a worker
certificate to the platform it registers for (see the threat model). Per-pool instance-name binding is possible:
`(starts_with(authenticationMetadata.private.sub, 'spiffe://cucina/worker/pool-a/') && contains(\`["tenant-a"]\`, instanceName)) || …`.

**Scheduler, `buildQueueStateGrpcServers`** (drain, kill; in-cluster only, R-SEC-4):

```text
validationJmespathExpression:
  length(uris) == `1` && uris[0] == 'spiffe://cucina/controller'
metadataExtractionJmespathExpression:
  {public: {user: uris[0]}, private: {sub: uris[0]}}
modifyDrainsAuthorizer / killOperationsAuthorizer:
  authenticationMetadata.private.sub == 'spiffe://cucina/controller'
```

**Storage shards** (optional defence in depth when the frontend dials them with mTLS; the frontend's server
certificate is then issued with clientAuth):
`` length(uris) == `1` && uris[0] == 'spiffe://cucina/server/frontend' ``.

**Host L2 `bb_storage`** (on the Mac, VM-facing; optional): accept only this host's VMs with
`` length(uris) == `1` && starts_with(uris[0], 'spiffe://cucina/worker/') && contains(uris[0], '/<SERIAL>/') `` (the host
renders its own serial).

### Verification rules for Cucina's own verifiers (Go)

`pki.Verifier` implements these; the `HostService` (mTLS) and every other Cucina component that accepts workload
certificates must use it rather than ad-hoc checks:

1. The chain verifies against the current CA bundle at the current time with EKU clientAuth.
2. The leaf has exactly one URI SAN and no DNS/IP/e-mail SANs; it parses as `spiffe://cucina/<role>/…` with the exact
   segment count and grammar of the table above (no user info, port, query, fragment, `.`/`..`, percent-encoding or
   empty segments).
3. The role is one the endpoint accepts (`HostService`: `host` only; `IssueVMIdentity` derives `<serial>` from the
   caller's host identity, never from the request).
4. Liveness is checked by the caller against the source of truth: a host identity is honoured only while its
   `MacHost` exists, is approved and is not `Denied`. Certificates are not revoked individually (no CRL/OCSP); they
   expire quickly and are not renewed. Urgent revocation adds the URI SAN to the deny-list's `sub` entries, which every
   certificate-facing Buildbarn authorizer checks (effective in ≤ 2–3 minutes, like JWT revocation).

### Server certificates and endpoint verification

* Server certificates carry the listener's DNS/IP SANs (service DNS names, `endpoints.serverName`, public names from
  values) plus `spiffe://cucina/server/<component>`. They are stored as `kubernetes.io/tls` Secrets with `ca.crt`;
  `tls.crt` holds the leaf followed by its issuing CA certificate, so a client that pins the CA (below) can find it in
  the handshake.
* Workers and hosts verify Cucina endpoints with the CA bundle (`ca_pem` of every enrollment/renewal response) and the
  server name `WorkerSettings.server_name`. Before their first enrollment they use the CA bundle **or an SPKI pin**
  delivered out of band (EC2: launch user data; Mac hosts: the MDM managed preferences). Neither is secret.
* **SPKI pin format**: `sha256:<64 lower-case hex>` = SHA-256 over the DER `SubjectPublicKeyInfo` of a CA certificate
  (`pki.SPKIPin`). A pinned client accepts a server chain only if some CA certificate in the presented chain has a
  pinned SPKI and the leaf verifies against it for the expected server name (`pki.PinnedTLSConfig`; several pins are
  allowed, which covers a CA rotation).

## PKI

### CA hierarchy and the CA Secret

One private CA per Cucina installation issues every workload and in-cluster certificate (`internal/pki`):

* **Chart-generated (default)**: an ECDSA P-256 self-signed root, 10-year validity, `pathLen 0` (it signs leaves only),
  key usage `certSign, cRLSign`. `cucina-controller bootstrap` (Helm pre-install/pre-upgrade hook, `pki.EnsureCA`)
  creates it in the Secret `config.pki.caSecret` (chart default `<release>-ca`) **if absent** and never overwrites an
  existing Secret, not even an unusable one (that is an error for the operator to resolve).
* **Existing Secret**: same layout, provided by the operator.
* **cert-manager**: a dedicated CA `Certificate` (`isCA: true`) whose Secret holds `tls.crt`/`tls.key` (the issuing,
  possibly intermediate, CA) and `ca.crt` (its root). Issued chains then carry the intermediate. The issuing CA must be
  dedicated to this installation (the trust domain `cucina` is not installation-specific).

| Secret key | Content |
| --- | --- |
| `ca.crt` | PEM **trust bundle**: one root, two during a rotation. Verifiers (Buildbarn `clientCertificateAuthorities`, workers, hosts, the controller) trust every certificate in it. |
| `ca.key` | PEM (PKCS#8, SEC 1 or PKCS#1) key of the **active** issuing CA; its certificate is the one in `ca.crt` with the matching public key, so bundle order never matters. |
| `next.key` | Only between the *introduce* and *activate* rotation phases: the key of the next CA. |
| `tls.crt`, `tls.key` | cert-manager form, used only when `ca.key` is absent. |

Every controller replica reads the CA Secret through the API and re-reads it every minute (`pki.SecretCASource`), so
rotation phases need no restart. Server certificates may instead come from cert-manager/ACME or an existing Secret: the
`Rotator` is then not configured, those Secrets (without Cucina's label) are never modified, and their CA can be added
to enrollment responses (`enroll.Options.ExtraTrustPEM`).

### Certificate profiles (the issuer decides every field)

| Field | Value |
| --- | --- |
| Serial | 128 random bits |
| Subject | `O=Cucina, OU=<role>, CN=<last identity segment>` — never taken from the CSR, carries no identity |
| Validity | `NotBefore = now − 5 min` (clock skew), `NotAfter = now + TTL`, capped at the issuing CA's expiry |
| Basic constraints | `CA:FALSE`, critical |
| Key usage | `digitalSignature` (+ `keyEncipherment` for RSA keys), critical |
| Extended key usage | `clientAuth` for worker, VM, host and controller; `serverAuth` (+ `clientAuth` only if the spec asks) for servers |
| SAN | exactly the identity URI; servers add their DNS/IP names |
| Key identifiers | SKI from the public key, AKI from the CA; no CRL distribution point, OCSP or AIA |

**CSR rules** (`pki.ParseCSR`): one PEM `CERTIFICATE REQUEST` block of at most 16 KiB; a valid self-signature (proof of
possession; no SHA-1/MD5); key ECDSA P-256/P-384, Ed25519 or RSA 2048–4096 bits with `e = 65537`; no requested
extension except a SAN holding exactly the expected identity URI — a CSR asking for `CA:TRUE`, key usages, EKUs or any
other SAN is **rejected**, not silently trimmed; the subject is ignored; the CA's own key is refused. The CSR
contributes only its public key; a property test checks that accepted CSRs with arbitrary subjects and SANs yield
certificates whose every field is the profile's.

### Lifetimes, renewal and metrics

| Certificate | TTL (hard max) | Renewal |
| --- | --- | --- |
| EC2 worker | 24 h (7 d; `pki.workerCertTTL`) | none: a new one at every boot; workers live ≤ 12 h (R-POOL-7) |
| macOS VM worker | 12 h (12 h; `pki.vmCertTTL`) | at every VM start, through its host |
| Mac host | 7 d (7 d; `pki.hostCertTTL`) | hostd calls `HostService.RenewCertificate` over mTLS at 2/3 of the lifetime |
| In-cluster server, controller client | 90 d (397 d) | leader-only `pki.Rotator` every 10 min: at 2/3 of the lifetime, on a SAN/usage change, and after a CA activation; Buildbarn reloads via `refreshInterval`, the controller via its file reloaders |

`cucina_cert_expiry_seconds{role}` is the time until the earliest expiry per role (negative once expired): `ca`,
`server`, `controller` in the controller; `host` in hostd; `worker` in the worker agent. Suggested alerts: `server` or
`controller` < 30 d (the Rotator failed), `ca` < 180 d (start a rotation), `host` < 1 d (renewal failing).

### CA rotation (two-root bundle) — runbook inputs

`pki.RotateCA(ctx, client, namespace, caSecret, phase)` applies one phase to the CA Secret (Cucina-managed CAs only;
cert-manager rotates its own CAs):

1. **introduce** — generates CA2, sets `ca.crt = CA1 + CA2` and `next.key`; CA1 keeps signing. Before the next phase,
   every verifier must trust CA2:
   * Buildbarn: `clientCertificateAuthorities` is inline PEM, so the chart's checksum annotation on the bundle rolls
     the frontend and scheduler pods;
   * controller replicas: ≤ 1 min; managed server Secrets get the new `ca.crt` within one Rotator pass (≤ 10 min);
   * EC2 workers: at their next boot (the whole fleet turns over within the 12 h maximum uptime);
   * Mac hosts: in every enrollment/renewal response (≤ 2/3 × 7 d ≈ 4.7 d), and new hosts through MDM: ship **both**
     CA certificates or pins in the managed preferences/trust profile before activating.
   Minimum wait: the longest of these (≈ 5 days unless hosts are made to renew earlier).
2. **activate** — `ca.key = next.key`: CA2 signs every new leaf; both roots stay trusted; the Rotator re-issues all
   managed server and controller certificates within ≤ 10 min.
3. **retire** — once the last CA1 leaf has expired (≥ 7 d host TTL + skew after activate), `ca.crt = CA2` only;
   Buildbarn rolls again. Remove CA1 from MDM profiles.

A **compromised CA key** cannot be rotated gracefully: create a new CA (introduce + activate + retire at once),
then re-enroll the fleet (EC2 workers re-enroll on their next launch; each Mac host needs `cucinactl hosts remove` +
re-approval because its old certificate can no longer authenticate a renewal).

## Enrollment

The `EnrollmentService` (`internal/enroll`) runs on every controller replica on the enrollment listener with **TLS
server authentication only** — callers have no certificate yet. All shared state lives in Kubernetes objects, so
replicas agree. Requests are rate-limited per source address (defaults: workers 120/min burst 60, hosts 60/min burst
30 — a site's Macs share one NAT address; the Service in front of the listener must preserve client addresses,
e.g. `externalTrafficPolicy: Local`, or every caller shares one bucket) and must carry a protocol version with major 1 (`internal/proto.Check`:
a mismatch is refused with a message naming the side to upgrade). Every decision is an audit log line
(`audit=true`, `event`, `result`, `source`, instance/serial/token id — never token material).

### EC2 workers — `EnrollWorker`, at every boot (R-SEC-3, R-POOL-3)

The agent sends the IMDS identity document, the base64 body of IMDS
`/latest/dynamic/instance-identity/rsa2048` (PKCS#7 SignedData, RSA-2048/SHA-256; the RSA-1024 `/signature` form is
refused) and a CSR for a key it generated and stored root-only **before** the call. The controller issues a worker
certificate only if, in order:

1. the PKCS#7 signature verifies with the embedded AWS RSA-2048 certificate of the document's Region (34 commercial
   Regions, from the AWS documentation; BER input accepted) and the sent document is byte-identical to the signed one;
2. `accountId` and `region` equal `aws.accountId`/`aws.region`; the zone belongs to the Region; `pendingTime` is at most
   20 min old (longer than the slowest `startupTimeout`) and not in the future; the reported architecture matches;
3. a **cluster-tag-filtered** `Compute.Describe` returns the instance `pending`/`running` with `cucina:managed-by`,
   `cucina:cluster`, `cucina:pool`, `cucina:generation`, `cucina:launch-token` (and `cucina:role=worker` if present),
   the document's image and zone, and a launch time within 2 min of `pendingTime`. An instance Describe does not show
   yet (EC2 eventual consistency right after `RunInstances`; the document is fresh) is answered `Unavailable` with a
   `RetryInfo` hint of 5 s — the agent retries for about 2 minutes — while a visible instance that is terminated,
   stopped or wrongly tagged is refused for good;
4. the launch token is one of the controller's own launch records: prefix of the instance's pool, the pool's current
   ledger epoch, a sequence number the autoscaler allocated (`enroll.LedgerLaunches` over the WorkerPool ledger);
5. the pool is configured (`PoolSettingsProvider.SettingsFor`) and the CSR passes the CSR rules;
6. **one enrollment per launch**: a Lease `cucina-enroll-<instance-id>` (label `cucina.sloper.ai/enrollment=worker-launch`,
   created atomically, pruned after 24 h) binds the launch to the first key. The same key may enroll again within
   10 min, at most 3 certificates per launch (agent crash after the response); another key, or anything later, is refused — so code
   running on the worker, which can read IMDS, cannot mint an identity, and a rebooted worker is replaced rather than
   re-admitted.

The response carries the certificate chain, the CA bundle, pool, generation and the `WorkerSettings`. Failures are
`Unauthenticated` (document/signature), `PermissionDenied` (definitive policy refusal), `FailedPrecondition` (pool,
protocol), `InvalidArgument` (CSR), `ResourceExhausted` (rate) or `Unavailable` (retry: instance not visible yet,
backend trouble; no internal detail is echoed). The agent verifies the enrollment endpoint with the CA PEM the
controller puts into the launch user data (`internal/workeragent/bootdata`).
Worker images should also block IMDS for the action user (`runCommandsAs`, R-SEC-5).

*Alternative, documented only:* an instance-role-scoped SSM SecureString read at boot. Rejected for v1: it is a
shared bearer secret every worker of the pool can read for as long as it exists, it needs SSM write access in the
controller and rotation, and it proves nothing about which instance asks.

### Mac hosts — `EnrollHost`, once per host (R-SEC-3, UC19, T14)

**Site enrollment tokens** (`cucinactl hosts enroll-token`): `cuc_et_<16 hex id>_<43 base64url secret>` (256-bit
secret). Only SHA-256 of the secret is stored, one JSON record per token in the Secret `<release>-enroll-tokens`
(compared in constant time). A token is multi-use, expires (default 7 d, max 90 d), is revocable, is bound to a site,
and has a maximum host count; every serial that first contacted Cucina with the token (pending or enrolled) counts
until the host is removed. Revoking or expiring a token never affects enrolled hosts (renewal uses mTLS, not tokens).

Hostd sends token + serial number + CSR (key kept in the System keychain) + facts. The answer depends on the
`MacHost` of that serial:

| MacHost state | Same key as bound | Other key |
| --- | --- | --- |
| none | `PENDING`; a Pending MacHost (`spec.approved=false`, site and reported labels) is created so an admin can approve it | — |
| pending (not approved) | `PENDING` (idempotent polling, `retry_after` 30 s) | `DENIED` (no takeover of a pending serial) |
| pre-registered or approved, never contacted | `APPROVED` + certificate (binds the key) | — |
| approved, key bound | `APPROVED` + certificate | `DENIED` |
| enrolled | `APPROVED` + fresh certificate (recovery after an outage, audited as `recovered`; ADR 0651) | `DENIED`: re-enrollment needs `cucinactl hosts remove` |

Token failures (malformed, unknown, wrong secret, expired, revoked, token site ≠ reported site or ≠ the host's
registered site, host count reached) answer `TOKEN_INVALID` and create nothing. `cucinactl hosts register <serial>…`
pre-registers and approves (e.g. serials pasted from Apple Business), `hosts approve` approves a pending host,
`hosts remove` deletes the MacHost, releases its token slot and deny-lists `spiffe://cucina/host/<serial>` until any
certificate it holds has expired.

**MacHost contract** (written by enrollment, read by the MacHost reconciler and hostlink): name = lower-case serial
for objects Cucina creates (others are found by `spec.serial`); `spec.serial` canonical upper case; `spec.site`,
`spec.labels`, `spec.approved`; label `cucina.sloper.ai/enroll-token=<token id>`; annotations
`cucina.sloper.ai/identity-key-sha256` (bound key: SHA-256 of the DER SubjectPublicKeyInfo),
`cucina.sloper.ai/pending-since`, `cucina.sloper.ai/enrolled-at`, `cucina.sloper.ai/hostname`; `status.certificateExpiry`.

**RBAC** the controller needs for enrollment and PKI in its namespace: Secrets get/create/update (CA, token store,
managed certificate Secrets); Leases get/list/create/update/delete; MacHosts get/list/create/update/delete and
`machosts/status` patch; WorkerPools get (launch ledger).

### Stronger optional path: MDM-issued device identity

The site token is world-readable on enrolled Macs (managed preferences); its exposure is contained by expiry,
revocation, the host count and serial admission. The stronger path, not implemented in v1, replaces the token with an
**MDM-issued ACME or SCEP identity** in the System keychain (not hardware-bound, so the root daemon can use it;
hardware-bound keys live in the data-protection keychain, which daemons cannot use, TN3137). Hostd would present it as
the TLS client certificate of `EnrollHost`; the controller would verify it against the MDM's CA and take the serial
from the certificate, keeping the approval step.

### Host-local certificate: the L2 listener

The host L2 `bb_storage` serves its VMs on the vmnet bridge (ADR 0413) and needs a server certificate. It does not
need Cucina's CA: hostd keeps a host-local CA (`pki.NewCAMaterial`, files readable by root only), issues the listener
certificate from it (`pki.NewIssuer(…).IssueServer(pub, "host-l2", nil, []net.IP{bridgeIP}, false, 0)`) and gives that
CA to its VMs as the storage trust anchor, while VMs keep authenticating to the L2 with their Cucina VM certificates.
No controller round trip and no new API are needed; the host already controls its VMs.

## Workload identity threat model

| Stolen | What the holder can do | Limits and response |
| --- | --- | --- |
| EC2 worker certificate + key (root on a worker) | Until expiry (≤ 24 h): synchronize with the scheduler and take actions of **any** platform queue under the instance names the synchronize authorizer allows (Buildbarn cannot bind a certificate to a platform), so read action inputs and return forged results; read/write CAS and write AC on the worker listener (cache poisoning). Cannot use the client endpoint (JWT only), drain/kill (controller only) or the management API. | Short TTL; one certificate per launch bound to the agent's key; deny-list the URI SAN (≤ 2–3 min) and terminate the instance. Pools are a trust boundary only across instance names (per-pool instance-name binding); for strict separation run one installation per trust level (R-SEC-5). |
| macOS VM certificate + key | As a worker, ≤ 12 h. | Issued only to its host for a VM the controller asked it to run. |
| Mac host certificate + key (root on the Mac) | ≤ 7 d, renewable while the MacHost exists: L2 upstream access (CAS read/write, AC read/write on the worker listener), VM identities for VMs the controller schedules on it, host stream. | `cucinactl hosts remove`: no renewal, no VM identities, URI SAN deny-listed. Root on a host already controls its VMs. |
| Site enrollment token (world-readable on every enrolled Mac) | Create Pending MacHosts for unknown serials (bounded by the host count, visible in `cucinactl hosts list`); claim a pre-registered serial that has not enrolled yet (serials are printed on the box) — the real host then sees `DENIED`, which is visible. Cannot affect enrolled hosts. | Short token TTL, revoke/rotate after each rollout wave, prefer approval over pre-registration where tokens may leak; MDM device identity (above). |
| Identity document + `rsa2048` signature (any process on an instance) | Nothing once the agent enrolled (key binding, 10-min window); a document from another instance fails the instance, account, freshness, Describe and ledger checks. | Block IMDS for the action user. |
| CA key (Secret in the release namespace) | Impersonate every workload and in-cluster server. | RBAC: only the controller and the bootstrap Job read it; etcd encryption at rest; compromise = new CA + fleet re-enrollment. |
| Controller client certificate | Drain/kill in the scheduler (in-cluster only). | Secret-protected, 90 d, rotated at 2/3. |

---

# Client authentication and tokens

Owned by the `auth` component (`internal/auth`, `internal/sts`, `internal/keys`). Sections: Deny-list, Cucina JWT,
Trust policies, Service accounts and the break-glass key, Signing keys and rotation, Threat model: tokens,
Revocation timings.

## Deny-list

Urgent revocation (R-AUTH-9). Buildbarn caches JWT validation results per token string and never re-validates a
cached token, so revocation happens in the **authorizers**, which run on every request. Every JMESPath authorizer
that admits JWT principals or workload certificates ANDs the deny fragment below.

### Where it lives

| Item | Value |
| --- | --- |
| ConfigMap | `config.Auth.DenyListConfigMap` (chart default `<release>-denylist`), release namespace |
| Writer | `cucina-controller` only (`internal/keys`). Created empty by `cucina-controller bootstrap` (Helm pre-install hook) before any Buildbarn pod starts. **The chart must not template it**: an upgrade would overwrite live revocations. |
| Key read by Buildbarn | `denylist.json` |
| Other keys | `revocations.json` (controller bookkeeping: reason, actor, times). Buildbarn never reads it. |
| Mount | as a **directory** (never `subPath`, which never updates), e.g. `/etc/cucina/denylist/`, in frontend and scheduler pods |
| Buildbarn config | every authorizer `jmespathExpression` gets `files: [{key: "denylist", path: "/etc/cucina/denylist/denylist.json"}]` |

### Why the check is a substring test

Buildbarn hands `files.<key>` to the expression as the **raw file text (a string)**, not as parsed JSON (bb-storage
`pkg/jmespath/expression.go`, `readFile`, at the pinned tag). `contains(files.denylist.sids, …)` therefore cannot
work. The check uses `contains()` on the string, and every entry is written as a quoted, prefixed *match token*:

* sid entries are the JSON string `"sid:<sid>"`, sub entries `"sub:<sub>"`;
* the needle is built the same way, quotes included, so `sub:google:12` never matches `"sub:google:123"` and a sid
  never matches a sub;
* `sid` and `sub` values only use characters JSON never escapes (below), so the encoded entry is byte-for-byte the
  needle; the writer refuses anything else;
* the file holds no other strings (no reasons, no timestamps), so free text can never produce a match.

### File shape (`denylist.json`)

```json
{"version":1,"sids":["sid:Zm9vYmFyYmF6cXV4MTIzND"],"subs":["sub:google:110248495921238986420","sub:spiffe://cucina/host/C02XK0AAJGH6"]}
```

* Empty file (written by bootstrap): `{"version":1,"sids":[],"subs":[]}`.
* `version` is the number `1`; entry order is not significant; compact JSON, UTF-8, no BOM.
* `sid`: exactly 22 characters of `[A-Za-z0-9_-]` (128 bits, base64url without padding).
* `sub`: `<scheme>:<rest>`, scheme `[a-z][a-z0-9-]{0,31}`, rest from `[A-Za-z0-9._~:@/+=%-]`, at most 512 bytes. The
  STS percent-encodes every other byte of a mapped subject (and `%` itself) when it mints a token, so the deny-list,
  the JWT `sub`, the audit log and `cucinactl` show the same string. Workload URI SANs (§Workload identity) already
  fit, so workers, hosts and VMs are revoked the same way.
* Size cap 64 KiB (> 1,000 entries). sid entries are pruned once every token that can carry them has expired
  (revocation time + maximum token TTL + 2 min skew); sub entries stay until removed or until their optional expiry.

### Expressions (the chart renders these verbatim)

JWT authenticator of the frontend client listener and of the scheduler (R-AUTH-4):

```text
claimsValidationJmespathExpression:
  payload.iss == '<stsIssuer>' && payload.aud == 'buildbarn' && type(payload.exp) == 'number' && type(payload.sub) == 'string' && type(payload.sid) == 'string' && type(payload.cucina) == 'object'

metadataExtractionJmespathExpression:
  {"public": {"user": payload.sub}, "private": merge(payload.cucina, {"sid": payload.sid, "sub": payload.sub})}
```

`<stsIssuer>` is `endpoints.stsUrl` without a trailing slash (the STS uses exactly that string as `iss` and as
`issuer` in the discovery document). `merge` adds `sid` and `sub` next to the verb lists, so verb checks keep the form
`authenticationMetadata.private.<verb>` (contracts §5.1) and certificate metadata (`private.sub` = URI SAN, see
§Workload identity) shares the same authorizers.

Deny fragment `<DENY>`:

```text
!contains(files.denylist, join('', ['"sid:', to_string(authenticationMetadata.private.sid), '"'])) && !contains(files.denylist, join('', ['"sub:', to_string(authenticationMetadata.private.sub), '"']))
```

| Authorizer | Expression |
| --- | --- |
| CAS `get`, `findMissing` | `contains(authenticationMetadata.private.cas_read, instanceName) && <DENY>` |
| CAS `put` | `contains(authenticationMetadata.private.cas_write, instanceName) && <DENY>` |
| AC `get` | `contains(authenticationMetadata.private.ac_read, instanceName) && <DENY>` |
| AC `put` | `contains(authenticationMetadata.private.ac_write, instanceName) && <DENY>` |
| `execute` (frontend and scheduler) | `contains(authenticationMetadata.private.execute, instanceName) && <DENY>` |
| scheduler `synchronize` | per §Workload identity, `&& <DENY>` |

Guaranteed behaviour (tested in `internal/keys` with `github.com/jmespath/go-jmespath` v0.4.0, the version
bb-storage pins, and end to end against the pinned `bb_storage` binary in `internal/sts`
`TestBuildbarnAcceptsCucinaTokens`, which also proves Buildbarn accepts the startup test vectors):

* deny-listed sid or sub → `false`; a sub that is a prefix or extension of a listed one → unaffected;
* a missing `files.denylist` key or a non-string file → expression error → Buildbarn denies; a missing file at start-up
  stops Buildbarn from starting, and a failed reload keeps the last good content;
* principals without `sid` (certificates) pass the sid half: `to_string(null)` is `"null"`, and `sid:null` / `sub:null`
  can never be written (charset);
* tokens without `sid`, `sub` or `cucina` fail claims validation (UNAUTHENTICATED).

The chart should render one Buildbarn `testVectors` entry per authorizer with a deny-listed sid (expected `false`), so a
mis-rendered expression aborts start-up instead of failing open. The expressions are exported from Go as
`keys.BuildbarnClaimsValidation(issuer)`, `keys.BuildbarnMetadataExtraction`, `keys.BuildbarnDenyFragment` and
`keys.BuildbarnAuthorizer(claimKey)`; `internal/keys` tests evaluate exactly these strings.

### Propagation time

The controller writes the ConfigMap at once (and the STS replicas stop minting for a deny-listed sub at once). The
kubelet projects ConfigMap updates within its sync period (about 60 s plus jitter); Buildbarn re-reads the file every
60 s. Worst case about 2–2.5 min, inside the R-AUTH-9 budget (T10 d: ≤ 3 min).

## Cucina JWT

Minted by the STS (`internal/keys.Minter`), validated locally by Buildbarn and by the management API (R-AUTH-3).

| Part | Value |
| --- | --- |
| Header | `{"alg":"ES256","kid":"<RFC 7638 thumbprint>","typ":"JWT"}`; compact serialization |
| `iss` | `endpoints.stsUrl` without trailing slash |
| `aud` | `"buildbarn"` (a plain string, never an array) |
| `sub` | the principal (below) |
| `iat`, `exp` | `exp − iat` = TTL: 15 min, or less when an applied grant has a smaller `maxTTL`. **No `nbf`** (Buildbarn applies zero leeway) |
| `jti` | 128 random bits, base64url (22 characters) |
| `sid` | session id, same format. Random per exchange for OIDC identities; for service keys `keys.SessionForKey(id)` (stable per key, so revoking the key deny-lists all its outstanding tokens) |
| `cucina` | `{"cas_read":[…],"cas_write":[…],"ac_read":[…],"ac_write":[…],"execute":[…],"admin":[…]}`: always all six keys, sorted unique instance names, `[]` when empty; `"*"` already expanded |
| `name` | optional display name (`claimMappings.displayName`, or the service account) for audit records only; ≤ 256 bytes, no control characters; Buildbarn ignores it and nothing authorizes on it |

Principals (`sub`) have the form `<scheme>:<id>` with scheme `[a-z][a-z0-9-]{0,31}`. The STS percent-encodes every
byte of a mapped subject outside `[A-Za-z0-9._~:@/+=-]` (and `%`), injectively: a GitHub workflow named `Build & test`
becomes `github:1401027334:Build%20%26%20test`. Service accounts are `sa:<account>`, the break-glass key `sa:break-glass`.

Buildbarn behaviour that shapes the design: it caches validation results per token string until LRU eviction and does
not flush on JWKS reload (hence deny-list in authorizers and frontend restarts for a compromised key); it re-reads the
JWKS file every 300 s and picks the key by `kid` (a token without `kid` is tried against every key — Cucina always sets it).

## STS (token exchange)

`cucina-controller sts` (2 replicas, stateless) serves on `listeners.sts` over TLS 1.2+ with HTTP/2; the certificate files
are re-read when they change. Endpoints: `GET /.well-known/cucina-configuration` (contracts §5.1; `identity_providers`
lists the `login` blocks of valid, enabled OIDC policies; defaults to scopes `openid email profile`, plus `offline_access`
for non-Google issuers), `POST /token`, `GET /jwks.json`, `GET /-/healthy`, `GET /-/ready` (ready once signing keys are loaded).

`POST /token` takes `application/x-www-form-urlencoded` parameters in the body only (a query string is refused), each at
most once, ≤ 64 KiB: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token`,
`subject_token_type`, optional `audience` (a configured instance name: the token is down-scoped to it) and
`requested_token_type` (access token or JWT). Actor tokens are refused.

| `subject_token_type` | Subject token |
| --- | --- |
| `urn:ietf:params:oauth:token-type:id_token` | OIDC ID token (Google, Entra, Keycloak, Dex, Okta, …) |
| `urn:ietf:params:oauth:token-type:jwt` | GitHub Actions OIDC token (`audience=cucina`) |
| `urn:cucina:params:oauth:token-type:service-key` (or `…:access_token`) | service-account key `cuc_sk_…` (`cucinactl login --key`) |

Success: `200 {"access_token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in"}`
with `Cache-Control: no-store`. Errors (RFC 6749 §5.2 body `{"error","error_description"}`; the description never
contains token material):

| HTTP | `error` | When |
| --- | --- | --- |
| 400 | `invalid_request` | malformed request, unknown `subject_token_type`, unknown `audience`, actor token |
| 400 | `unsupported_grant_type` | `grant_type` is not token exchange |
| 400 | `invalid_grant` | subject token malformed, bad signature, `alg` none/HMAC, unknown `kid`, expired, `nbf`/`iat` > 1 min in the future, issuer not trusted, audience not accepted, `jti` replayed; service key unknown, wrong, revoked or expired |
| 403 | `access_denied` | token valid but a claim validation rule fails, no grant applies, the policies disagree on the subject, or the principal is deny-listed |
| 429 | `slow_down` | rate limit (`auth.rateLimitPerMinute` per client IP and per subject, default 60), with `Retry-After` |
| 500 | `server_error` | internal failure, issuer discovery/JWKS unavailable, group lookup failed (fail closed) |

Every exchange writes one audit record (JSON, `"event":"sts.exchange"`): result, issuer, remote IP, subject, matching
policies and applied grants, and on success the scopes, `sid`, `jti` and TTL — never a token. Metrics:
`cucina_sts_exchanges_total{issuer,result}` (issuer = a configured issuer URL, `service-account` or `unknown`, so the label
stays bounded) and the histogram `cucina_sts_token_ttl_seconds`.

The `jti` replay cache (policies with `requireUniqueJTI`) is per replica: a stolen GitHub token could be exchanged once
more against the other replica within its ~5 min lifetime, yielding the same principal's grants.

## Trust policies

A `TrustPolicy` (CRD, `api/v1alpha1`) is evaluated by `internal/auth.Engine` (R-AUTH-2):

1. The token's unverified `iss` selects the candidate policies (issuer `url` or `additionalIssuers`, matching
   `subjectTokenType`); none → `invalid_grant`.
2. Each candidate's issuer verifies the token with go-oidc: discovery from `discoveryURL` or `<url>/.well-known/openid-configuration`
   (its `issuer` must equal `url`; `jwks_uri` must be https; optional private CA; no redirects), JWKS cached and re-fetched on
   an unknown `kid`, an asymmetric algorithm advertised by the issuer (`none` and HMAC never), exact `iss`, `aud` containing an
   accepted audience, `exp` without leeway, `nbf`/`iat` at most 1 min ahead.
3. All `claimValidationRules` must be true; `claimMappings` give the subject (required), display name and groups;
   `groupLookup` adds groups (Cloud Identity `searchDirectGroups` keyed on the verified e-mail, cached 1–10 min, default
   10 min; a failed lookup fails the exchange).
4. The grants whose `condition` (over `claims`, `subject`, `groups`) is true apply; `"*"` expands to `instanceNames`.
5. The union over all matching policies is the principal; their subjects must agree; the TTL is the smallest `maxTTL` of
   the applied grants (≤ 15 min). A token matching no policy, or with no applicable grant, is rejected.

CEL: compiled when the policy is loaded (type errors, a non-bool rule or condition, a non-string subject, an unknown
verb, a `maxTTL` above 15 min, a non-https issuer make the policy `Valid=False` with the reason; it never matches);
expression ≤ 4096 characters, nesting ≤ 64, run-time cost ≤ 1,000,000, interrupt checks every 100 iterations, 100 ms
per evaluation. Every evaluation error fails closed: a rule or mapping error denies the policy, a condition error skips
that grant. Rules see `claims` only; conditions also `subject` and `groups`. The `ext.Strings` library is available.

Shipped samples (`internal/auth/testdata/policies/`, tested; the chart's values mirror them):

| File | Shows |
| --- | --- |
| `google-workspace.yaml` | Google Desktop-app login: `iss` ∈ {`https://accounts.google.com`, `accounts.google.com`}, `aud` = client id, `email_verified` bool or `"true"`, `hd` ∈ allow-list (never the e-mail domain), principal `google:<sub>`; admins by verified, `hd`-pinned e-mail |
| `github-sloper-ai-cucina.yaml` | **this repository**: `repository_owner_id == "310369022" && repository_id == "1401027334"`, never `sub`; events push/merge_group/schedule/workflow_dispatch (+ pull_request read-only), `pull_request_target` denied, only this repo's workflows (`job_workflow_ref`), GitHub-hosted runners, `jti` replay cache; `ac-write` only for pushes to protected `refs/heads/main` |
| `github-actions-example.yaml` | generic GitHub: pinned reusable workflow, `ac-write` for protected refs or a reviewer-gated `environment` |
| `entra.yaml`, `keycloak.yaml`, `dex.yaml`, `okta.yaml` | other OIDC providers: tenant pinning and `oid` (Entra; its `email` is unverified), group paths (Keycloak), connector pinning (Dex), registered redirect ports (Okta) |
| `service-account.yaml` | a `serviceAccount` policy with `maxTTL` |

Fork pull requests get no GitHub OIDC token by default; when they do, claims cannot tell a fork from a branch, so every
`pull_request` is limited to `cas-read`/`ac-read`.

## Service accounts and the break-glass key

Opt-in long-lived keys for systems without OIDC (R-AUTH-10): `cuc_sk_<id>_<secret>`, id 16 and secret 52 characters of
lower-case base32 (80 + 260 random bits). The Secret `auth.serviceKeysSecret` holds one JSON record per key id (account,
description, creator, creation/expiry/last-use/revocation times) and `HMAC-SHA-256(pepper, id, secret)`; the 32-byte
pepper lives in the signing-key Secret (ADR 0601). Keys authenticate only through a `TrustPolicy` of type
`serviceAccount` naming the account; the principal is `sa:<account>`. Every authentication reads the store from the API
server, so `RevokeServiceKey` is effective for new exchanges at once; it also deny-lists the key's `sid`, which stops
its outstanding tokens within 3 min. Last use is recorded at most every 5 min per key and replica. Go API for the
management service: `keys.Manager.{CreateServiceKey, ListServiceKeys, RevokeServiceKey, Revoke, ListRevocations}`.

Break-glass (R-AUTH-12): `cucina-controller bootstrap` (Helm pre-install hook) calls `keys.EnsureBreakGlass`, which
creates the Secret `auth.breakGlassKeySecret` with entries `key` (the plaintext key) and `key-id` when it is absent and
registers only its hash. **The chart must not template this Secret** (if an operator pre-creates it, its `key` must be a
valid `cuc_sk_` key; bootstrap registers it). Retrieve it with
`kubectl -n <ns> get secret <release>-break-glass -o jsonpath='{.data.key}' | base64 -d` and use `cucinactl login --key`.
The key gets a built-in policy granting every verb on every instance name; a valid `TrustPolicy` with
`serviceAccount: {name: break-glass, breakGlass: true}` replaces (for example narrows) it. Once OIDC admins exist, disable
it with `cucinactl keys revoke <key-id>` (a revoked break-glass key stays revoked across upgrades) or rotate it
(`keys.RotateBreakGlass`: new key in the Secret, old key revoked).

## Signing keys and rotation

| Object | Content |
| --- | --- |
| Secret `auth.signingKeySecret` | `state.json` (rotation state: per kid `pending`/`active`/`retiring` and times), `<kid>.pem` (PKCS#8 P-256 private key), `service-key-pepper` |
| ConfigMap `auth.jwksConfigMap` | `jwks.json`: public keys of every published key (`kid`, `alg: ES256`, `use: sig`). Written by the controller; mounted **as a directory** (never `subPath`) in frontends and scheduler: `jwksFile: /etc/cucina/jwks/jwks.json`. Not templated by the chart |
| ConfigMap `auth.denyListConfigMap` | §Deny-list |

`keys.EnsureSigningKeys` (bootstrap hook) creates all three when absent (first key active at once — no frontend has loaded
anything yet) and never overwrites. Replicas re-read the Secret every 30 s (`KeyRing`). The leader's `keys.Rotator`
(`Reconcile` every 30 s) implements R-AUTH-9:

1. `StartRotation` publishes K2 (pending) next to K1; the JWKS ConfigMap is written before the publication time is recorded.
2. After `keyRotationPublishLead` (≥ 10 min) **and** once the `LoadVerifier` hook confirms every frontend and the scheduler
   loaded K2 (wired by the controller, for example with a probe token from `Minter.MintWithKey(K2)`), K2 becomes active and K1
   retiring. Without a verifier the rotation waits (unless `AllowUnverifiedPromotion`).
3. K1 stays published for max TTL + 2 min skew + 30 s replica refresh after the promotion, then is removed.

Compromised key: `Rotator.Compromise(kid | "*")` removes the key from the Secret and the JWKS at once (a fresh key becomes
active immediately if needed) and calls the `FrontendRestarter` hook, because Buildbarn keeps accepting cached tokens
until restarted. Outstanding tokens of the removed key fail after the restart; clients renew transparently.

## Management API authorization

`auth.NewInterceptor(verifier, auth.ManagementRequirements(), instanceNames)` (unary and stream) validates the Cucina JWT
locally (ES256 by a published `kid`, exact `iss`, `aud`, `exp` without leeway, deny-list snapshot) and attaches the
caller (`auth.CallerFromContext`). Methods named `Get*`, `List*`, `Watch*`, `Stream*`, `Collect*` need `execute` or
`admin` on at least one instance name, except the sensitive reads of ADR 0580 (worker logs, host diagnostics, support
bundle, enrollment-token, service-key and revocation listings); those and every other method need `admin` on **every**
configured instance name (cluster administrator); methods missing from the table are denied. The management service
enforces the same classes itself (`internal/mgmt`, ADR 0580); the interceptor is defence in depth. Missing/invalid/expired/revoked token →
`UNAUTHENTICATED`; insufficient verbs → `PERMISSION_DENIED`.

## Kubernetes objects and RBAC (auth part)

| Who | Needs (release namespace) |
| --- | --- |
| bootstrap hook | Secrets: get, create, update (signing keys, service keys, break-glass); ConfigMaps: get, create (JWKS, deny-list) |
| controller (leader + management API) | Secrets: get, update (signing keys, service keys); ConfigMaps: get, create, update (JWKS, deny-list); TrustPolicies: get, list, watch, status update |
| STS Deployment | Secrets: get (signing keys, service keys), update (service keys: last use); ConfigMaps: get (deny-list); TrustPolicies: get, list, watch |
| frontend, scheduler pods | mount JWKS and deny-list ConfigMaps as directories (no API access) |

Restrict with `resourceNames`. Nobody else needs these Secrets; reading the signing-key Secret is equivalent to
minting any token.

## Threat model: tokens

| Attacker has | Can | Limits and response |
| --- | --- | --- |
| A Cucina JWT (stolen from a laptop or CI log) | Everything its `cucina` lists allow, from anywhere that reaches the endpoint, until `exp` (≤ 15 min); management reads (and mutations if `admin` on all instances) | Short TTL; `RevokePrincipal` by `sid` (that session) or `sub` (≤ 3 min); cannot renew without the IdP credential |
| An external ID token (Google) | Exchange it repeatedly at the STS until its `exp` (≈ 1 h) for 15-min JWTs | `aud` pins our client, `hd` + `email_verified` pin the domain; revoke the `sub` (new exchanges refused at once); disable the account at the IdP (effective within one TTL after the ID token expires) |
| A GitHub Actions OIDC token | One exchange per STS replica within its ~5 min life, with the grants of that repository/event/ref | Owner/repo ids, event allow-list, `pull_request_target` denial, workflow and runner pinning; `jti` replay cache; `ac-write` only for protected-main pushes |
| Write access to a branch, or a fork PR | Branch workflows: `cas-write` + `execute` (no `ac-write`); PRs: cache reads only | Cache poisoning needs `ac-write`; remote execution runs on pool workers — pools are the trust boundary (R-SEC-5) |
| A service-account key | Its policy's grants, indefinitely until revoked or expired | Opt-in, scoped, hashed at rest, `RevokeServiceKey` (immediate + outstanding tokens ≤ 3 min), last-use audit; prefer OIDC |
| The break-glass key | Cluster admin | Readable only with Secret access (already cluster-admin); rotate or revoke once OIDC admins exist |
| The signing-key Secret | Mint any token for any principal | Compromised path (remove + restart frontends); RBAC `resourceNames`; rotate |
| Write access to ConfigMaps or TrustPolicies | Add a JWKS key, clear the deny-list, grant itself | Equivalent to cluster admin; protect with RBAC and audit Kubernetes writes |
| Pathological CEL in a policy or huge claims | Slow the STS | Size/nesting limits, cost limit, timeout, 16 KiB token cap, rate limits |

## Revocation timings

| Action | New exchanges refused | Existing Cucina JWTs stop working |
| --- | --- | --- |
| Account disabled at the IdP | when its ID/refresh tokens stop working | ≤ 15 min after the last exchange |
| TrustPolicy edited, disabled or deleted | when the STS reloads policies (watch: seconds) | ≤ 15 min (combine with the deny-list) |
| `RevokePrincipal` sub | at once on the replica that wrote it, ≤ 10 s on the others | ≤ 3 min (kubelet sync + 60 s reload) |
| `RevokePrincipal` sid | — (OIDC sids are per exchange) | ≤ 3 min |
| `RevokeServiceKey` | at once (store read per exchange) | ≤ 3 min (key's sid deny-listed) |
| Group removed (Cloud Identity) | ≤ cache TTL (≤ 10 min) | + ≤ 15 min |
| Signing key compromised | at once (new active key) | when the frontends and scheduler have restarted |
| Management API | — | deny-list: at once on the writing replica, ≤ 10 s elsewhere |

## Rotation runbook inputs

For `docs/operations/` (owned by the operations docs):

* **Planned signing-key rotation.** Trigger `Rotator.StartRotation` (controller subcommand or management call, wired by the
  controller). Watch the controller log for `signing key promotion waiting` (reason: lead time, `not loaded by every frontend
  yet`, verification errors) and `signing key promoted`; old key removal follows ≈ 17.5 min later. No client action; builds
  continue (T10 e). Optional schedule: `keys.Options.RotateEvery`.
* **Compromised signing key.** `Rotator.Compromise(<kid>|"*")`; confirm the kid is gone from `jwks.json`, the frontends and
  scheduler restarted, and new tokens validate. Expect one failed request per client before transparent renewal.
* **Revoke a person, CI workflow or session.** `cucinactl` revoke by `sub` (from the audit log) or `sid`; effective ≤ 3 min;
  also remove the IdP access or edit the TrustPolicy so they cannot come back after removal. Remove deny-list entries for
  subjects that no longer need them (64 KiB cap).
* **Service keys.** Create per system with a TTL; review `last_used`; revoke unused keys. The pepper cannot be rotated in
  place: rotating it invalidates every service key (create new keys first).
* **Break-glass.** Retrieve with kubectl (NOTES.txt), rotate after use, revoke once OIDC admins exist.
* **Issuer changes.** A new IdP signing key needs nothing (JWKS re-fetched on unknown `kid`); a private IdP CA goes into
  `issuer.certificateAuthority`; changing `url`/`discoveryURL` takes effect on the next policy reload.
