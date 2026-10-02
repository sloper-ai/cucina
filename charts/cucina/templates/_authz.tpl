{{/* SPDX-License-Identifier: FSL-1.1-ALv2 */}}
{{/*
Buildbarn authentication and authorization (R-AUTH-4, R-AUTH-9, R-SEC-2/-4, R-CACHE-5).
The expressions are the ones docs/security.md specifies ("Verification rules for
Buildbarn" and "Deny-list"), rendered verbatim:

  * JWT clients: metadata {"public": {"user": sub}, "private": merge(cucina, {sid, sub})},
    so authenticationMetadata.private.<verb> holds the explicit instance-name lists of the
    token (cas_read, cas_write, ac_read, ac_write, execute);
  * workers and hosts (frontend worker listener): certificate metadata with the same verb
    lists rendered from values (per pool where a pool restricts its instance names) plus
    private.sub = the URI SAN, so one set of authorizers serves both kinds of principal
    (internal/pki.BuildbarnWorkerListenerMetadata); the File System Access Cache is a
    worker-only store: its authorizers also require a worker or host sub;
  * scheduler worker and BuildQueueState listeners: private.sub only;
  * every authorizer ANDs the deny fragment over files.denylist (raw file text; entries are
    the quoted match tokens "sid:<sid>" / "sub:<sub>").
Every expression carries JMESPath test vectors (buildbarn.authorizerTestVectors): Buildbarn
refuses to start when one fails, so a mis-rendered expression never fails open.
*/}}

{{- define "cucina.bb.spiffe.worker" -}}spiffe://cucina/worker/{{- end -}}
{{- define "cucina.bb.spiffe.host" -}}spiffe://cucina/host/{{- end -}}

{{- define "cucina.bb.expr.deny" -}}
!contains(files.denylist, join('', ['"sid:', to_string(authenticationMetadata.private.sid), '"'])) && !contains(files.denylist, join('', ['"sub:', to_string(authenticationMetadata.private.sub), '"']))
{{- end -}}

{{/* Instance names per pool for certificate principals: pools that restrict
     spec.instanceNames get their own list, everything else (other pools, CR-only pools,
     hosts) gets every configured instance name. */}}
{{- define "cucina.bb.poolInstanceNames" -}}
{{- $out := list -}}
{{- range $pool := include "cucina.pools" . | fromJsonArray -}}
{{- if $pool.spec.instanceNames -}}
{{- $out = append $out (dict "pool" $pool.name "instanceNames" $pool.spec.instanceNames) -}}
{{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{/* The certificate instance-name list as a JMESPath expression over uris[0]. */}}
{{- define "cucina.bb.expr.certInstanceNames" -}}
{{- $all := printf "`%s`" (toJson .Values.instanceNames) -}}
{{- $clauses := list -}}
{{- range include "cucina.bb.poolInstanceNames" . | fromJsonArray -}}
{{- $clauses = append $clauses (printf "(starts_with(uris[0], '%s%s/') && `%s`)" (include "cucina.bb.spiffe.worker" $) .pool (toJson .instanceNames)) -}}
{{- end -}}
{{- if $clauses -}}
({{ join " || " (append $clauses $all) }})
{{- else -}}
{{ $all }}
{{- end -}}
{{- end -}}

{{/* What the frontend's certificate metadata expression yields for a URI (test vectors). Arg: list $ uri */}}
{{- define "cucina.bb.certMetadata" -}}
{{- $ctx := index . 0 -}}
{{- $uri := index . 1 -}}
{{- $names := $ctx.Values.instanceNames -}}
{{- $matched := false -}}
{{- range include "cucina.bb.poolInstanceNames" $ctx | fromJsonArray -}}
{{- if and (not $matched) (hasPrefix (printf "%s%s/" (include "cucina.bb.spiffe.worker" $ctx) .pool) $uri) -}}
{{- $names = .instanceNames -}}
{{- $matched = true -}}
{{- end -}}
{{- end -}}
{{- toJson (dict "public" (dict "user" $uri) "private" (dict "sub" $uri "cas_read" $names "cas_write" $names "ac_read" $names "ac_write" $names "execute" (list))) -}}
{{- end -}}

{{/* Principals and deny-list content used by the test vectors (never real identities). */}}
{{- define "cucina.bb.testPrincipals" -}}
{{- $in := list (first .Values.instanceNames) -}}
{{- $none := list -}}
{{- $jwt := dict "cas_read" $in "cas_write" $in "ac_read" $in "ac_write" $in "execute" $in "admin" $none -}}
{{- $ro := dict "cas_read" $in "cas_write" $none "ac_read" $in "ac_write" $none "execute" $none "admin" $none -}}
{{- $w := include "cucina.bb.spiffe.worker" . -}}
{{- $h := include "cucina.bb.spiffe.host" . -}}
{{- $workerURI := printf "%stest-pool/i-0123456789abcdef0" $w -}}
{{- $hostURI := printf "%sTESTSERIAL1" $h -}}
{{- $revokedHostURI := printf "%sTESTREVOKED" $h -}}
{{- $c := .Values.buildbarn.scheduler.controllerIdentity -}}
{{- toJson (dict
  "client" (dict "public" (dict "user" "google:test-client") "private" (merge (dict "sid" "test-sid-ok" "sub" "google:test-client") $jwt))
  "reader" (dict "public" (dict "user" "github:test-reader") "private" (merge (dict "sid" "test-sid-ro" "sub" "github:test-reader") $ro))
  "revokedSid" (dict "public" (dict "user" "google:test-client") "private" (merge (dict "sid" "test-sid-revoked" "sub" "google:test-client") $jwt))
  "revokedSub" (dict "public" (dict "user" "google:test-revoked") "private" (merge (dict "sid" "test-sid-other" "sub" "google:test-revoked") $jwt))
  "worker" (include "cucina.bb.certMetadata" (list . $workerURI) | fromJson)
  "host" (include "cucina.bb.certMetadata" (list . $hostURI) | fromJson)
  "revokedHost" (include "cucina.bb.certMetadata" (list . $revokedHostURI) | fromJson)
  "schedulerWorker" (dict "public" (dict "user" $workerURI) "private" (dict "sub" $workerURI))
  "schedulerHost" (dict "public" (dict "user" $hostURI) "private" (dict "sub" $hostURI))
  "controller" (dict "public" (dict "user" $c) "private" (dict "sub" $c))
  "denylist" (toJson (dict "version" 1 "sids" (list "sid:test-sid-revoked") "subs" (list "sub:google:test-revoked" (printf "sub:%s" $revokedHostURI))))
  "instance" (first .Values.instanceNames)
  "foreign" "test-not-a-configured-instance") -}}
{{- end -}}

{{/*
A jmespathExpression authorizer: "<expr> && <DENY>", the deny-list file, test vectors.
Args: dict "ctx" $ "expr" <expression> "vectors" <list of [principal, instance, expected]>
*/}}
{{- define "cucina.bb.authorizer" -}}
{{- $ctx := .ctx -}}
{{- $p := include "cucina.bb.testPrincipals" $ctx | fromJson -}}
{{- $paths := include "cucina.paths" $ctx | fromYaml -}}
{{- $e := dict "expression" (printf "%s && %s" .expr (include "cucina.bb.expr.deny" $ctx)) "files" (list (dict "key" "denylist" "path" $paths.denyList)) -}}
{{- if $ctx.Values.buildbarn.authorizerTestVectors -}}
{{- $vectors := list -}}
{{- range .vectors -}}
{{- $input := dict "authenticationMetadata" (index $p (index . 0)) "instanceName" (index $p (index . 1)) "files" (dict "denylist" $p.denylist) -}}
{{- $vectors = append $vectors (dict "input" $input "expectedOutput" (index . 2)) -}}
{{- end -}}
{{- $_ := set $e "testVectors" $vectors -}}
{{- end -}}
{{- toJson (dict "jmespathExpression" $e) -}}
{{- end -}}

{{/* Every authorizer, keyed by role (docs/security.md, Deny-list table). */}}
{{- define "cucina.bb.authorizers" -}}
{{- $ctx := . -}}
{{- $verbVectors := dict
  "cas_read"  (list (list "client" "instance" true) (list "reader" "instance" true) (list "client" "foreign" false) (list "revokedSid" "instance" false) (list "revokedSub" "instance" false) (list "worker" "instance" true) (list "worker" "foreign" false) (list "host" "instance" true) (list "revokedHost" "instance" false))
  "cas_write" (list (list "client" "instance" true) (list "reader" "instance" false) (list "revokedSid" "instance" false) (list "worker" "instance" true) (list "host" "instance" true) (list "revokedHost" "instance" false))
  "ac_read"   (list (list "client" "instance" true) (list "reader" "instance" true) (list "revokedSub" "instance" false) (list "worker" "instance" true) (list "host" "instance" true))
  "ac_write"  (list (list "client" "instance" true) (list "reader" "instance" false) (list "client" "foreign" false) (list "revokedSid" "instance" false) (list "revokedSub" "instance" false) (list "worker" "instance" true) (list "worker" "foreign" false) (list "host" "instance" true) (list "revokedHost" "instance" false))
  "execute"   (list (list "client" "instance" true) (list "reader" "instance" false) (list "client" "foreign" false) (list "revokedSid" "instance" false) (list "revokedSub" "instance" false) (list "worker" "instance" false) (list "host" "instance" false))
-}}
{{- $out := dict -}}
{{- range $verb, $vectors := $verbVectors -}}
{{- $_ := set $out $verb (include "cucina.bb.authorizer" (dict "ctx" $ctx "expr" (printf "contains(authenticationMetadata.private.%s, instanceName)" $verb) "vectors" $vectors) | fromJson) -}}
{{- end -}}
{{- $workload := printf "(starts_with(authenticationMetadata.private.sub, '%s') || starts_with(authenticationMetadata.private.sub, '%s'))" (include "cucina.bb.spiffe.worker" $ctx) (include "cucina.bb.spiffe.host" $ctx) -}}
{{- $fsacVectors := list (list "worker" "instance" true) (list "host" "instance" true) (list "worker" "foreign" false) (list "revokedHost" "instance" false) (list "client" "instance" false) -}}
{{- $_ := set $out "fsac_read" (include "cucina.bb.authorizer" (dict "ctx" $ctx "expr" (printf "%s && contains(authenticationMetadata.private.cas_read, instanceName)" $workload) "vectors" $fsacVectors) | fromJson) -}}
{{- $_ := set $out "fsac_write" (include "cucina.bb.authorizer" (dict "ctx" $ctx "expr" (printf "%s && contains(authenticationMetadata.private.cas_write, instanceName)" $workload) "vectors" $fsacVectors) | fromJson) -}}
{{- $_ := set $out "synchronize" (include "cucina.bb.authorizer" (dict "ctx" $ctx
  "expr" (printf "starts_with(authenticationMetadata.private.sub, '%s')" (include "cucina.bb.spiffe.worker" $ctx))
  "vectors" (list (list "schedulerWorker" "instance" true) (list "schedulerHost" "instance" false) (list "controller" "instance" false) (list "client" "instance" false))) | fromJson) -}}
{{- $_ := set $out "controller" (include "cucina.bb.authorizer" (dict "ctx" $ctx
  "expr" (printf "authenticationMetadata.private.sub == '%s'" $ctx.Values.buildbarn.scheduler.controllerIdentity)
  "vectors" (list (list "controller" "instance" true) (list "schedulerWorker" "instance" false) (list "client" "instance" false))) | fromJson) -}}
{{- toJson $out -}}
{{- end -}}

{{/* JWT authentication policy of the frontend client listener and the scheduler client listener. Args: list $ cacheSize */}}
{{- define "cucina.bb.jwtPolicy" -}}
{{- $ctx := index . 0 -}}
{{- $paths := include "cucina.paths" $ctx | fromYaml -}}
{{- $iss := include "cucina.stsUrl" $ctx -}}
{{- $aud := $ctx.Values.auth.audience -}}
{{- $claimsExpr := dict "expression" (printf "payload.iss == '%s' && payload.aud == '%s' && type(payload.exp) == 'number' && type(payload.sub) == 'string' && type(payload.sid) == 'string' && type(payload.cucina) == 'object'" $iss $aud) -}}
{{- $metaExpr := dict "expression" "{\"public\": {\"user\": payload.sub}, \"private\": merge(payload.cucina, {\"sid\": payload.sid, \"sub\": payload.sub})}" -}}
{{- if $ctx.Values.buildbarn.authorizerTestVectors -}}
{{- $grants := dict "cas_read" (list "main") "cas_write" (list) "ac_read" (list "main") "ac_write" (list) "execute" (list "main") "admin" (list) -}}
{{- $good := dict "iss" $iss "aud" $aud "exp" 4102444800 "iat" 4102443900 "sub" "google:test-client" "sid" "test-sid" "jti" "test-jti" "cucina" $grants -}}
{{- $_ := set $claimsExpr "testVectors" (list
  (dict "input" (dict "payload" $good) "expectedOutput" true)
  (dict "input" (dict "payload" (merge (dict "iss" "https://attacker.example") $good)) "expectedOutput" false)
  (dict "input" (dict "payload" (merge (dict "aud" (list $aud)) $good)) "expectedOutput" false)
  (dict "input" (dict "payload" (omit $good "exp")) "expectedOutput" false)
  (dict "input" (dict "payload" (omit $good "sid")) "expectedOutput" false)
  (dict "input" (dict "payload" (omit $good "cucina")) "expectedOutput" false)) -}}
{{- /* A token cannot smuggle sid/sub through its cucina claim: merge() puts the real ones last. */ -}}
{{- $smuggling := merge (dict "cucina" (merge (dict "sid" "forged" "sub" "spiffe://cucina/controller") $grants)) $good -}}
{{- $_ := set $metaExpr "testVectors" (list (dict "input" (dict "payload" $smuggling) "expectedOutput" (dict
  "public" (dict "user" "google:test-client")
  "private" (merge (dict "sid" "test-sid" "sub" "google:test-client") $grants)))) -}}
{{- end -}}
{{- toJson (dict "jwt" (dict
  "jwksFile" $paths.jwks
  "maximumCacheSize" (index . 1)
  "cacheReplacementPolicy" "LEAST_RECENTLY_USED"
  "claimsValidationJmespathExpression" $claimsExpr
  "metadataExtractionJmespathExpression" $metaExpr)) -}}
{{- end -}}

{{/*
mTLS authentication policy. Args: list $ role, role is "workers+hosts" (frontend worker
listener), "workers" (scheduler worker listener) or "controller" (scheduler
BuildQueueState listener). The CA bundle is injected with Jsonnet importstr
(placeholder, see cucina.bb.render).
*/}}
{{- define "cucina.bb.mtlsPolicy" -}}
{{- $ctx := index . 0 -}}
{{- $role := index . 1 -}}
{{- $w := include "cucina.bb.spiffe.worker" $ctx -}}
{{- $h := include "cucina.bb.spiffe.host" $ctx -}}
{{- $c := $ctx.Values.buildbarn.scheduler.controllerIdentity -}}
{{- $subOnly := "{public: {user: uris[0]}, private: {sub: uris[0]}}" -}}
{{- $workerURI := printf "%stest-pool/i-0123456789abcdef0" $w -}}
{{- $vmURI := printf "%stest-pool/TESTSERIAL1/vm-1" $w -}}
{{- $hostURI := printf "%sTESTSERIAL1" $h -}}
{{- $validation := "" -}}
{{- $metadata := $subOnly -}}
{{- $accept := list -}}
{{- $reject := list -}}
{{- if eq $role "workers+hosts" -}}
{{- $validation = printf "length(uris) == `1` && (starts_with(uris[0], '%s') || starts_with(uris[0], '%s'))" $w $h -}}
{{- $l := include "cucina.bb.expr.certInstanceNames" $ctx -}}
{{- $metadata = printf "{public: {user: uris[0]}, private: {sub: uris[0], cas_read: %s, cas_write: %s, ac_read: %s, ac_write: %s, execute: `[]`}}" $l $l $l $l -}}
{{- $accept = list $workerURI $vmURI $hostURI -}}
{{- range include "cucina.bb.poolInstanceNames" $ctx | fromJsonArray -}}
{{- $accept = append $accept (printf "%s%s/i-0123456789abcdef0" $w .pool) -}}
{{- end -}}
{{- $reject = list $c "spiffe://cucina/server/frontend" "spiffe://other/worker/test-pool/i-0123456789abcdef0" -}}
{{- else if eq $role "workers" -}}
{{- $validation = printf "length(uris) == `1` && starts_with(uris[0], '%s')" $w -}}
{{- $accept = list $workerURI $vmURI -}}
{{- $reject = list $hostURI $c -}}
{{- else if eq $role "controller" -}}
{{- $validation = printf "length(uris) == `1` && uris[0] == '%s'" $c -}}
{{- $accept = list $c -}}
{{- $reject = list $workerURI $hostURI "spiffe://cucina/server/controller" -}}
{{- else -}}
{{- fail (printf "unknown mTLS role %q" $role) -}}
{{- end -}}
{{- $validationExpr := dict "expression" $validation -}}
{{- $metadataExpr := dict "expression" $metadata -}}
{{- if $ctx.Values.buildbarn.authorizerTestVectors -}}
{{- $none := list -}}
{{- $vectors := list (dict "input" (dict "dnsNames" $none "emailAddresses" $none "uris" $none) "expectedOutput" false) -}}
{{- $metaVectors := list -}}
{{- range $accept -}}
{{- $vectors = append $vectors (dict "input" (dict "dnsNames" $none "emailAddresses" $none "uris" (list .)) "expectedOutput" true) -}}
{{- $expected := dict "public" (dict "user" .) "private" (dict "sub" .) -}}
{{- if eq $role "workers+hosts" -}}{{- $expected = include "cucina.bb.certMetadata" (list $ctx .) | fromJson -}}{{- end -}}
{{- $metaVectors = append $metaVectors (dict "input" (dict "dnsNames" $none "emailAddresses" $none "uris" (list .)) "expectedOutput" $expected) -}}
{{- end -}}
{{- range $reject -}}
{{- $vectors = append $vectors (dict "input" (dict "dnsNames" $none "emailAddresses" $none "uris" (list .)) "expectedOutput" false) -}}
{{- end -}}
{{- $vectors = append $vectors (dict "input" (dict "dnsNames" $none "emailAddresses" $none "uris" (list (first $accept) (first $accept))) "expectedOutput" false) -}}
{{- $_ := set $validationExpr "testVectors" $vectors -}}
{{- $_ := set $metadataExpr "testVectors" $metaVectors -}}
{{- end -}}
{{- toJson (dict "tlsClientCertificate" (dict
  "clientCertificateAuthorities" "@@IMPORTSTR_CA@@"
  "validationJmespathExpression" $validationExpr
  "metadataExtractionJmespathExpression" $metadataExpr)) -}}
{{- end -}}
