<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0400 — Buildbarn configuration: Helm-rendered protojson, `importstr` for CA bundles, startup self-checks

* Status: accepted (2026-10-02)

## Context
R-CP-2 wants every control-plane configuration rendered from values as protojson for the pinned releases (ADR 0001),
no Jsonnet for users, and a check that boots the pinned binaries. Two facts of the pinned protos shape the rendering:
`tlsClientCertificate.clientCertificateAuthorities` accepts only inline PEM (no file path), and Buildbarn evaluates its
configuration file with Jsonnet before strict `protojson.Unmarshal`. The CA bundle is created by the bootstrap hook after
Helm renders the templates, so its PEM is unknown at render time.

## Decision
* Templates build each configuration as a dict and print it with `toPrettyJson`; the CA field is the one non-JSON
  construct: `importstr "/cucina/ca/ca.crt"` (the `ca.crt` item of the CA Secret, mounted as a directory). ConfigMap
  keys are `*.jsonnet` to say so. Users never write Jsonnet.
* Every JMESPath expression (JWT claims/metadata, certificate validation/metadata, every authorizer) carries
  `testVectors`; Buildbarn refuses to start when one fails, so a mis-rendered authorizer can never fail open.
* `charts/cucina/tests/boot` renders 24 profiles (size × storage mode × client exposure × TLS source), parses the
  scheduler configuration with the pinned bb-remote-execution Go types, boots each distinct configuration with the
  pinned `bb_storage`/`bb_scheduler`, and runs a wired small profile end to end. It is the gate for every Buildbarn bump.

## Consequences
A CA bundle change (rotation) needs a rolling restart of the Buildbarn pods; leaf certificates rotate without restarts
(`refreshInterval`). Configurations are larger (test vectors), still far below the ConfigMap limit (~75 KB for the frontend).
