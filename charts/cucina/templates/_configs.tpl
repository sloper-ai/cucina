{{/* SPDX-License-Identifier: FSL-1.1-ALv2 */}}
{{/* Rendered configurations (named templates live in _*.tpl so every template, and helm-unittest, can use them). */}}

{{/*
Frontend: stateless bb_storage (R-CP-1). Two listeners: clients (TLS + JWT) and
workers/hosts (mTLS). Shards to storage, routes Execute to the scheduler,
advertises ZSTD, caches FindMissingBlobs, checks AC completeness.
*/}}
{{- define "cucina.frontend.config" -}}
{{- $ports := include "cucina.ports" . | fromYaml -}}
{{- $paths := include "cucina.paths" . | fromYaml -}}
{{- $eff := include "cucina.effective" (list . "frontend") | fromYaml -}}
{{- $authz := include "cucina.bb.authorizers" . | fromJson -}}
{{- $maxMsg := .Values.buildbarn.maximumMessageSizeBytes -}}
{{- $keepalive := include "cucina.bb.keepalive" . | fromJson -}}
{{- $sharding := include "cucina.bb.sharding" . | fromJson -}}

{{- $client := dict
  "listenAddresses" (list (printf ":%d" (int $ports.frontendClient)))
  "authenticationPolicy" (include "cucina.bb.jwtPolicy" (list . $eff.jwtCacheSize) | fromJson)
  "maximumReceivedMessageSizeBytes" $maxMsg
  "keepaliveEnforcementPolicy" $keepalive -}}
{{- if include "cucina.clientTLSAtFrontend" . -}}
{{- $_ := set $client "tls" (include "cucina.bb.serverTLS" (list . $paths.tlsClients) | fromJson) -}}
{{- end -}}
{{- $worker := dict
  "listenAddresses" (list (printf ":%d" (int $ports.frontendWorker)))
  "authenticationPolicy" (include "cucina.bb.mtlsPolicy" (list . "workers+hosts") | fromJson)
  "tls" (include "cucina.bb.serverTLS" (list . $paths.tlsWorkers) | fromJson)
  "maximumReceivedMessageSizeBytes" $maxMsg
  "keepaliveEnforcementPolicy" $keepalive -}}

{{- $ac := dict "completenessChecking" (dict
  "backend" $sharding
  "maximumTotalTreeSizeBytes" .Values.buildbarn.frontend.maximumTotalTreeSizeBytes) -}}
{{- with .Values.buildbarn.frontend.actionCachePurgeBefore }}
{{- /* AC purge (R-CACHE-5): hide worker-produced results completed before this time. */ -}}
{{- $ac = dict "actionResultExpiring" (dict "backend" $ac "minimumTimestamp" . "minimumValidity" "315360000s" "maximumValidityJitter" "1s") -}}
{{- end }}

{{- $scheduler := dict
  "address" (printf "%s:%d" (include "cucina.fqdn" (list . (include "cucina.scheduler.name" .))) (int $ports.schedulerClient))
  "addMetadataJmespathExpression" (dict "expression" "{\"build.bazel.remote.execution.v2.requestmetadata-bin\": incomingGRPCMetadata.\"build.bazel.remote.execution.v2.requestmetadata-bin\", \"authorization\": incomingGRPCMetadata.authorization}") -}}

{{- $cfg := dict
  "grpcServers" (list $client $worker)
  "schedulers" (dict "" (dict "endpoint" $scheduler))
  "maximumMessageSizeBytes" $maxMsg
  "global" (include "cucina.bb.global" . | fromJson)
  "contentAddressableStorage" (dict
    "backend" (dict "existenceCaching" (dict
      "backend" $sharding
      "existenceCache" (dict
        "cacheSize" $eff.existenceCacheSize
        "cacheDuration" .Values.buildbarn.frontend.existenceCacheDuration
        "cacheReplacementPolicy" "LEAST_RECENTLY_USED")))
    "getAuthorizer" $authz.cas_read
    "putAuthorizer" $authz.cas_write
    "findMissingAuthorizer" $authz.cas_read)
  "actionCache" (dict
    "backend" $ac
    "getAuthorizer" $authz.ac_read
    "putAuthorizer" $authz.ac_write)
  "fileSystemAccessCache" (dict
    "backend" $sharding
    "getAuthorizer" $authz.fsac_read
    "putAuthorizer" $authz.fsac_write)
  "executeAuthorizer" $authz.execute
  "supportedCompressors" (list "ZSTD")
  "zstdPool" (include "cucina.bb.zstdPool" (list . $eff.zstd.maximumEncoders $eff.zstd.maximumDecoders) | fromJson) -}}
{{- include "cucina.bb.render" (list . $cfg) -}}
{{- end -}}

{{/*
Storage shards: bb_storage `local` backends with persistent state on one PVC per
shard (R-CP-1/-3): CAS, AC, ISCC and FSAC. In-cluster only (frontend and
scheduler), plaintext and uncompressed (P3); optional NetworkPolicy restricts the
gRPC port to those pods.
*/}}
{{- define "cucina.storage.config" -}}
{{- $ports := include "cucina.ports" . | fromYaml -}}
{{- $maxMsg := .Values.buildbarn.maximumMessageSizeBytes -}}
{{- $allow := dict "allow" (dict) -}}
{{- $cfg := dict
  "grpcServers" (list (dict
    "listenAddresses" (list (printf ":%d" (int $ports.storage)))
    "authenticationPolicy" (dict "allow" (dict))
    "maximumReceivedMessageSizeBytes" $maxMsg))
  "maximumMessageSizeBytes" $maxMsg
  "global" (include "cucina.bb.global" . | fromJson)
  "contentAddressableStorage" (dict
    "backend" (include "cucina.bb.local" (list . "cas") | fromJson)
    "getAuthorizer" $allow "putAuthorizer" $allow "findMissingAuthorizer" $allow)
  "actionCache" (dict
    "backend" (include "cucina.bb.local" (list . "ac") | fromJson)
    "getAuthorizer" $allow "putAuthorizer" $allow)
  "initialSizeClassCache" (dict
    "backend" (include "cucina.bb.local" (list . "iscc") | fromJson)
    "getAuthorizer" $allow "putAuthorizer" $allow)
  "fileSystemAccessCache" (dict
    "backend" (include "cucina.bb.local" (list . "fsac") | fromJson)
    "getAuthorizer" $allow "putAuthorizer" $allow)
  "zstdPool" (include "cucina.bb.zstdPool" (list . 8 8) | fromJson) -}}
{{- include "cucina.bb.render" (list . $cfg) -}}
{{- end -}}

{{/*
PVC sizes derived from the store layout: the filesystem PVC holds the stores plus 10%
(filesystem overhead, ext4 reserved blocks) and 1 GiB; the block-mode CAS device holds
its blocks (bb_storage sizes them from the device). StatefulSet volumeClaimTemplates are
immutable, so an installed release keeps its live claim sizes as long as the stores still
fit them (with the 10%), and fails with the procedure otherwise (docs/operations/chart.md).
*/}}
{{- define "cucina.storage.pvcSizes" -}}
{{- $layout := include "cucina.bb.storeLayout" . | fromJson -}}
{{- $gi := 1073741824 -}}
{{- $block := eq .Values.storage.mode "block" -}}
{{- $fs := 0 -}}
{{- range $store, $l := $layout -}}
{{- if not (and $block (eq $store "cas")) -}}
{{- $fs = add $fs (int64 $l.blocksBytes) -}}
{{- end -}}
{{- $fs = add $fs (int64 $l.klmBytes) -}}
{{- end -}}
{{- $fsMin := div (mul $fs 11) 10 -}}
{{- $out := dict "filesystem" (printf "%dGi" (add (div (add $fsMin (sub $gi 1)) $gi) 1)) -}}
{{- $min := dict "filesystem" $fsMin -}}
{{- if $block -}}
{{- $_ := set $out "cas" (printf "%dGi" (div (add (int64 $layout.cas.blocksBytes) (sub $gi 1)) $gi)) -}}
{{- $_ := set $min "cas" (int64 $layout.cas.blocksBytes) -}}
{{- end -}}
{{- with .Values.storage.persistence.size -}}
{{- if lt (include "cucina.bytes" . | int64) (include "cucina.bytes" $out.filesystem | int64) -}}
{{- fail (printf "storage.persistence.size %s is smaller than the stores need (%s); grow the PVC or shrink storage.stores" . $out.filesystem) -}}
{{- end -}}
{{- $_ := set $out "filesystem" . -}}
{{- end -}}
{{- $live := lookup "apps/v1" "StatefulSet" .Release.Namespace (include "cucina.storage.name" .) -}}
{{- if $live -}}
{{- $claims := dict -}}
{{- range ($live.spec).volumeClaimTemplates -}}
{{- $_ := set $claims .metadata.name (((.spec).resources).requests).storage -}}
{{- end -}}
{{- $fsClaim := ternary "meta" "data" $block -}}
{{- $want := ternary (list "cas" "meta") (list "data") $block -}}
{{- if ne (keys $claims | sortAlpha | join ",") (join "," $want) -}}
{{- fail (printf "storage.mode cannot change after install: the installed StatefulSet has the claims [%s], storage.mode=%s needs [%s] (reinstall the storage with a new cache)" (keys $claims | sortAlpha | join ", ") .Values.storage.mode (join ", " $want)) -}}
{{- end -}}
{{- $liveFs := get $claims $fsClaim -}}
{{- with .Values.storage.persistence.size -}}
{{- if ne (include "cucina.bytes" .) (include "cucina.bytes" $liveFs) -}}
{{- fail (printf "storage.persistence.size %s differs from the installed PVCs (%s: %s): StatefulSet claim templates are immutable; expand the PVCs, delete the StatefulSet with --cascade=orphan, then upgrade (docs/operations/chart.md#upgrade)" . $fsClaim $liveFs) -}}
{{- end -}}
{{- end -}}
{{- range $claim, $size := $claims -}}
{{- $key := ternary "cas" "filesystem" (eq $claim "cas") -}}
{{- if lt (include "cucina.bytes" $size | int64) (get $min $key | int64) -}}
{{- fail (printf "the storage PVCs (%s) are %s but the stores need %s: StatefulSet claim templates are immutable; shrink storage.stores, or expand the PVCs, delete the StatefulSet with --cascade=orphan and set storage.persistence.size (docs/operations/chart.md#upgrade)" $claim $size (get $out $key)) -}}
{{- end -}}
{{- $_ := set $out $key $size -}}
{{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{/*
Scheduler: bb_scheduler (bb-remote-execution 20260930T173749Z-1a3be95). Three
listeners: clients (the frontend forwards Execute with the caller's JWT, which is
validated again here, R-AUTH-4), workers (mTLS, synchronize authorizer, R-SEC-2) and
BuildQueueState (mTLS, controller identity only, R-SEC-4). Predeclared queues for
every pool platform make Execute queue at zero workers (R-RE-2, ADR 0002).
*/}}
{{- define "cucina.scheduler.config" -}}
{{- $ports := include "cucina.ports" . | fromYaml -}}
{{- $paths := include "cucina.paths" . | fromYaml -}}
{{- $authz := include "cucina.bb.authorizers" . | fromJson -}}
{{- $s := .Values.buildbarn.scheduler -}}
{{- $maxMsg := .Values.buildbarn.maximumMessageSizeBytes -}}
{{- $keepalive := include "cucina.bb.keepalive" . | fromJson -}}
{{- $sharding := include "cucina.bb.sharding" . | fromJson -}}
{{- $eff := include "cucina.effective" (list . "frontend") | fromYaml -}}
{{- $analyzer := dict "defaultExecutionTimeout" $s.defaultExecutionTimeout "maximumExecutionTimeout" $s.maximumExecutionTimeout -}}
{{- if $s.feedbackDrivenSizeClasses -}}
{{- $_ := set $analyzer "feedbackDriven" (dict "failureCacheDuration" "86400s" "historySize" 32) -}}
{{- end -}}
{{- $cfg := dict
  "clientGrpcServers" (list (dict
    "listenAddresses" (list (printf ":%d" (int $ports.schedulerClient)))
    "authenticationPolicy" (include "cucina.bb.jwtPolicy" (list . $eff.jwtCacheSize) | fromJson)
    "maximumReceivedMessageSizeBytes" $maxMsg))
  "workerGrpcServers" (list (dict
    "listenAddresses" (list (printf ":%d" (int $ports.schedulerWorker)))
    "authenticationPolicy" (include "cucina.bb.mtlsPolicy" (list . "workers") | fromJson)
    "tls" (include "cucina.bb.serverTLS" (list . $paths.tlsServer) | fromJson)
    "maximumReceivedMessageSizeBytes" $maxMsg
    "keepaliveEnforcementPolicy" $keepalive))
  "buildQueueStateGrpcServers" (list (dict
    "listenAddresses" (list (printf ":%d" (int $ports.schedulerBuildQueueState)))
    "authenticationPolicy" (include "cucina.bb.mtlsPolicy" (list . "controller") | fromJson)
    "tls" (include "cucina.bb.serverTLS" (list . $paths.tlsServer) | fromJson)
    "maximumReceivedMessageSizeBytes" $maxMsg))
  "contentAddressableStorage" $sharding
  "maximumMessageSizeBytes" $maxMsg
  "global" (include "cucina.bb.global" . | fromJson)
  "predeclaredPlatformQueues" (include "cucina.queues" . | fromJsonArray)
  "executeAuthorizer" $authz.execute
  "synchronizeAuthorizer" $authz.synchronize
  "modifyDrainsAuthorizer" $authz.controller
  "killOperationsAuthorizer" $authz.controller
  "actionRouter" (dict "simple" (dict
    "platformKeyExtractor" (dict "action" (dict))
    "invocationKeyExtractors" (list
      (dict "authenticationMetadata" (dict))
      (dict "correlatedInvocationsId" (dict))
      (dict "toolInvocationId" (dict)))
    "initialSizeClassAnalyzer" $analyzer))
  "platformQueueWithNoWorkersTimeout" $s.platformQueueWithNoWorkersTimeout
  "zstdPool" (include "cucina.bb.zstdPool" (list . 8 8) | fromJson) -}}
{{- if $s.feedbackDrivenSizeClasses -}}
{{- $_ := set $cfg "initialSizeClassCache" $sharding -}}
{{- end -}}
{{- include "cucina.bb.render" (list . $cfg) -}}
{{- end -}}
