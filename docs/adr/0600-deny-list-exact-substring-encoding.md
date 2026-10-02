<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0600 — Deny-list: quoted match tokens tested with `contains()` on the raw file

* Status: accepted (2026-10-02)

## Context
R-AUTH-9 revokes urgently through a deny-list file read by every Buildbarn JMESPath authorizer (authorizers are not
cached; JWT validation results are). Contracts §5.1 sketches `!contains(deny_list.sid, authenticationMetadata.private.sid)`.
At the pinned bb-storage tag, `jmespath.Expression.files` are exposed as the **raw file text** (`readFile` returns a
string), so `files.denylist.sids` is not addressable. The metadata extraction in §5.1 (`"private": payload.cucina`) also
does not carry `sid`.

## Decision
* `denylist.json` is `{"version":1,"sids":["sid:<sid>",…],"subs":["sub:<sub>",…]}` and nothing else. Authorizers AND
  `!contains(files.denylist, join('', ['"sid:', to_string(authenticationMetadata.private.sid), '"'])) && !contains(files.denylist, join('', ['"sub:', to_string(authenticationMetadata.private.sub), '"']))`.
  Quotes and the `sid:`/`sub:` prefixes make the substring test an exact, namespaced match.
* Values are restricted to characters JSON never escapes (sid: 22 × `[A-Za-z0-9_-]`; sub: `<scheme>:` +
  `[A-Za-z0-9._~:@/+=%-]`, ≤ 512 bytes); the STS percent-encodes mapped subjects into that set; the writer refuses
  anything else and caps the file at 64 KiB.
* Metadata extraction becomes `{"public": {"user": payload.sub}, "private": merge(payload.cucina, {"sid": payload.sid, "sub": payload.sub})}`,
  matching the certificate metadata of §Workload identity (`private.sub` = URI SAN), so one fragment serves JWTs and
  certificates. The expressions are Go constants in `internal/keys` and tested with go-jmespath v0.4.0.

## Consequences
* Contracts §5.1 needs the new metadata-extraction expression (additive; the JWT claims are unchanged).
* No prefix/wildcard revocation; revoke each subject (or disable the TrustPolicy).
* Every authorizer scans the file per request: kept small by pruning sid entries after max TTL + skew and by the cap.
