<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0573 — EC2 boot data: versioned JSON in user data, no TOFU, tolerant of new fields

* Status: accepted (2026-10-02)

## Context
R-POOL-3/R-SEC-3: a worker must find and trust the EnrollmentService without any baked secret and without
trust-on-first-use. Instance tags (readable through IMDS) carry the pool, generation and launch token but cannot hold a CA
bundle (tag values are limited to 256 characters). EC2 user data is readable by every process on the instance, so it must stay
public. Windows AMIs run EC2Launch v2, which interprets YAML user data with top-level `version`/`tasks` keys; cloud-init ignores
unknown non-multipart payloads. The controller (`internal/controller`) and the agent baked into an AMI are different builds, so
the format must survive version skew in both directions.

## Decision
`internal/workeragent/bootdata` defines `BootData{Version, EnrollEndpoint, ServerName, CAPEM, Cluster, Pool, Generation}`
as compact JSON of at most 4 KiB. The version key is `cucinaBootData` (no `version`/`tasks` keys, so EC2Launch v2 leaves the
document alone). `Encode` refuses any PEM block that is not a certificate (a CA key in user data would be readable by every
action). The agent verifies the controller's certificate against `caPem` and `serverName` and powers off on mismatch. Decoding
ignores unknown fields; optional additions keep version 1, anything an old agent must not ignore bumps the version, which old
agents refuse with an explicit "rebuild the worker image" error.

## Consequences
* The controller imports `bootdata.Encode` to fill `ports.LaunchRequest.UserData`; the agent's `--boot-data FILE` reads the same
  format outside EC2.
* Two CA certificates (rotation) fit easily with ECDSA P-256 roots; RSA-4096 bundles may hit the 4 KiB limit.
* Unknown fields are not an error, unlike Cucina's strict configuration parsing: boot data crosses binary versions.
