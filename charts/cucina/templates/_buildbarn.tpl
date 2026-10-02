{{/* SPDX-License-Identifier: FSL-1.1-ALv2 */}}
{{/*
Buildbarn configuration building blocks. Every helper prints JSON (consume with
fromJson) that matches the protos of the pinned releases exactly (ADR 0001):
bb-storage 20260930T153215Z-086b011 for every bb_storage process and
bb-remote-execution 20260930T173749Z-1a3be95 (bb-storage ae61334 protos) for
bb_scheduler. Field names are protojson (lowerCamelCase). Authentication and
authorization live in _authz.tpl.
*/}}

{{/* Server TLS from a mounted kubernetes.io/tls Secret directory (rotated via refreshInterval). */}}
{{- define "cucina.bb.serverTLS" -}}
{{- $ctx := index . 0 -}}
{{- $dir := index . 1 -}}
{{- toJson (dict "serverKeyPair" (dict "files" (dict
  "certificatePath" (printf "%s/tls.crt" $dir)
  "privateKeyPath" (printf "%s/tls.key" $dir)
  "refreshInterval" $ctx.Values.tls.refreshInterval))) -}}
{{- end -}}

{{/* Keepalive enforcement for listeners clients and workers ping (Bazel pings every 60 s). */}}
{{- define "cucina.bb.keepalive" -}}
{"minTime": "20s", "permitWithoutStream": true}
{{- end -}}

{{/* global: diagnostics HTTP server (/-/healthy, /metrics). */}}
{{- define "cucina.bb.global" -}}
{{- $ports := include "cucina.ports" . | fromYaml -}}
{{- toJson (dict "diagnosticsHttpServer" (dict
  "httpServers" (list (dict "listenAddresses" (list (printf ":%d" (int $ports.diagnostics))) "authenticationPolicy" (dict "allow" (dict))))
  "enablePrometheus" true
  "enablePprof" false
  "enableActiveSpans" false)) -}}
{{- end -}}

{{/* Addresses of the storage shards (StatefulSet pods behind the headless Service). */}}
{{- define "cucina.bb.shardAddresses" -}}
{{- $ports := include "cucina.ports" . | fromYaml -}}
{{- $eff := include "cucina.effective" (list . "storage") | fromYaml -}}
{{- $name := include "cucina.storage.name" . -}}
{{- $out := list -}}
{{- range $i := until (int $eff.shards) -}}
{{- $out = append $out (printf "%s-%d.%s:%d" $name $i (include "cucina.fqdn" (list $ $name)) (int $ports.storage)) -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{/* sharding backend over every storage shard (in-cluster hop: uncompressed, R-DATA-3). */}}
{{- define "cucina.bb.sharding" -}}
{{- $shards := dict -}}
{{- range $i, $addr := include "cucina.bb.shardAddresses" . | fromJsonArray -}}
{{- $_ := set $shards (toString $i) (dict "backend" (dict "grpc" (dict "client" (dict "address" $addr))) "weight" 1) -}}
{{- end -}}
{{- toJson (dict "sharding" (dict "shards" $shards)) -}}
{{- end -}}

{{/* Bounded zstd pool. Args: list $ maximumEncoders maximumDecoders */}}
{{- define "cucina.bb.zstdPool" -}}
{{- $ctx := index . 0 -}}
{{- $z := $ctx.Values.buildbarn.frontend.zstd -}}
{{- toJson (dict
  "maximumEncoders" (index . 1)
  "maximumDecoders" (index . 2)
  "encoderWindowSizeBytes" $z.encoderWindowSizeBytes
  "decoderWindowSizeBytes" $z.decoderWindowSizeBytes
  "encoderLevel" $z.encoderLevel) -}}
{{- end -}}

{{/*
Per-store layout of a storage shard (R-CP-3), derived from one size per store:
blocks old 8 / current 24 / new 3 (CAS) or 1 (others), spare 3; key-location map
of keyLocationMapFactor x (size / averageObjectSize) entries of 66 bytes.
Prints JSON {store: {blocksBytes, klmBytes, newBlocks}}.
*/}}
{{- define "cucina.bb.storeLayout" -}}
{{- $eff := include "cucina.effective" (list . "storage") | fromYaml -}}
{{- $bb := .Values.buildbarn.storage -}}
{{- $out := dict -}}
{{- range $store := list "cas" "ac" "fsac" "iscc" -}}
{{- $size := include "cucina.bytes" (index $eff.stores $store).size | int64 -}}
{{- $avg := include "cucina.bytes" (index $bb.averageObjectSize $store) | int64 -}}
{{- $entries := div (mul $size (int64 $bb.keyLocationMapFactor)) $avg -}}
{{- $klm := mul (max $entries 1024) 66 -}}
{{- $_ := set $out $store (dict "blocksBytes" $size "klmBytes" $klm "newBlocks" (ternary 3 1 (eq $store "cas"))) -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{/* A `local` blob access for one store of a storage shard (new nested keyLocationMap schema). Args: list $ store */}}
{{- define "cucina.bb.local" -}}
{{- $ctx := index . 0 -}}
{{- $store := index . 1 -}}
{{- $paths := include "cucina.paths" $ctx | fromYaml -}}
{{- $layout := index (include "cucina.bb.storeLayout" $ctx | fromJson) $store -}}
{{- $metaDir := ternary $paths.storageMeta $paths.storageData (eq $ctx.Values.storage.mode "block") -}}
{{- $blocksSource := dict "file" (dict "path" (printf "%s/%s-blocks" $paths.storageData $store) "sizeBytes" $layout.blocksBytes) -}}
{{- if and (eq $ctx.Values.storage.mode "block") (eq $store "cas") -}}
{{- $blocksSource = dict "devicePath" $paths.casDevice -}}
{{- else if eq $ctx.Values.storage.mode "block" -}}
{{- $blocksSource = dict "file" (dict "path" (printf "%s/%s-blocks" $metaDir $store) "sizeBytes" $layout.blocksBytes) -}}
{{- end -}}
{{- toJson (dict "local" (dict
  "keyLocationMap" (dict
    "onBlockDevice" (dict "file" (dict "path" (printf "%s/%s-key-location-map" $metaDir $store) "sizeBytes" $layout.klmBytes))
    "maximumGetAttempts" 16
    "maximumPutAttempts" 64)
  "oldBlocks" 8
  "currentBlocks" 24
  "newBlocks" $layout.newBlocks
  "blocksOnBlockDevice" (dict
    "source" $blocksSource
    "spareBlocks" 3
    "dataIntegrityValidationCache" (dict "cacheSize" 100000 "cacheDuration" "14400s" "cacheReplacementPolicy" "LEAST_RECENTLY_USED"))
  "persistent" (dict
    "stateDirectoryPath" (printf "%s/%s" $paths.storageState $store)
    "minimumEpochInterval" $ctx.Values.buildbarn.storage.minimumEpochInterval))) -}}
{{- end -}}

{{/*
Render a Buildbarn configuration dict to the ConfigMap payload: protojson JSON in
which the CA bundle placeholder becomes `importstr "<path>"` (the pinned protos only
accept inline PEM for client-certificate authorities; Buildbarn evaluates its
configuration file with Jsonnet, so this is the one non-JSON construct; ADR 0400).
Args: list $ config-dict
*/}}
{{- define "cucina.bb.render" -}}
{{- $ctx := index . 0 -}}
{{- $paths := include "cucina.paths" $ctx | fromYaml -}}
{{- toPrettyJson (index . 1)
  | replace "\"@@IMPORTSTR_CA@@\"" (printf "importstr %s" (quote $paths.caBundle))
  | replace "\\u0026" "&"
  | replace "\\u003c" "<"
  | replace "\\u003e" ">" -}}
{{- end -}}
