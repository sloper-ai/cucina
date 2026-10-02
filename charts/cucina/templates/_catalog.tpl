{{/* SPDX-License-Identifier: FSL-1.1-ALv2 */}}
{{/*
Platform catalog and pools (docs/contracts.md §3, ADR 0002).
  cucina.catalog  : files/platforms.json (a copy of platforms/pools.json, kept equal by
                    a test) + values.platforms.extra, as JSON. The controller reads the
                    same merged file (config.Controller.platformsFile).
  cucina.pools    : values.pools validated against the catalog (fail fast) and
                    normalised: [{name, spec}] for enabled pools.
  cucina.queues   : predeclaredPlatformQueues for every (instance name × runner ×
                    size class) of every pool platform, deduplicated and sorted.
*/}}
{{- define "cucina.catalog" -}}
{{- $catalog := .Files.Get "files/platforms.json" | fromJson -}}
{{- if not $catalog.platforms -}}
{{- fail "files/platforms.json is missing or empty" -}}
{{- end -}}
{{- $names := dict -}}
{{- range $catalog.platforms -}}
{{- $_ := set $names .name true -}}
{{- end -}}
{{- $platforms := $catalog.platforms -}}
{{- range .Values.platforms.extra -}}
{{- if hasKey $names .name -}}
{{- fail (printf "platforms.extra: platform %q already exists in the catalog" .name) -}}
{{- end -}}
{{- $_ := set $names .name true -}}
{{- $platforms = append $platforms . -}}
{{- end -}}
{{- $_ := set $catalog "platforms" $platforms -}}
{{- toJson $catalog -}}
{{- end -}}

{{- define "cucina.pools" -}}
{{- $catalog := include "cucina.catalog" . | fromJson -}}
{{- $byName := dict -}}
{{- range $catalog.platforms -}}
{{- $_ := set $byName .name . -}}
{{- end -}}
{{- $seen := dict -}}
{{- $out := list -}}
{{- range $pool := .Values.pools -}}
{{- if hasKey $seen $pool.name -}}
{{- fail (printf "pools: duplicate pool name %q" $pool.name) -}}
{{- end -}}
{{- $_ := set $seen $pool.name true -}}
{{- if ne (toString $pool.enabled) "false" -}}
{{- $platform := index $byName $pool.platform -}}
{{- if not $platform -}}
{{- fail (printf "pools[%s]: platform %q is not in the catalog (platforms/pools.json + platforms.extra)" $pool.name $pool.platform) -}}
{{- end -}}
{{- if ne $pool.provider $platform.provider -}}
{{- fail (printf "pools[%s]: provider %q does not match platform %q (provider %q)" $pool.name $pool.provider $pool.platform $platform.provider) -}}
{{- end -}}
{{- $sizeClassName := default "default" $pool.sizeClass -}}
{{- $sizeClass := "" -}}
{{- range $platform.sizeClasses -}}
{{- if eq .name $sizeClassName -}}{{- $sizeClass = .sizeClass -}}{{- end -}}
{{- end -}}
{{- if eq (toString $sizeClass) "" -}}
{{- fail (printf "pools[%s]: size class %q is not declared by platform %q" $pool.name $sizeClassName $pool.platform) -}}
{{- end -}}
{{- range $pool.instanceNames -}}
{{- if not (has . $.Values.instanceNames) -}}
{{- fail (printf "pools[%s]: instance name %q is not in instanceNames" $pool.name .) -}}
{{- end -}}
{{- end -}}
{{- $spec := omit $pool "name" "enabled" -}}
{{- $out = append $out (dict "name" $pool.name "spec" $spec "sizeClass" $sizeClass "runners" $platform.runners "instanceNames" (default $.Values.instanceNames $pool.instanceNames)) -}}
{{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{- define "cucina.queues" -}}
{{- $queues := dict -}}
{{- range $pool := include "cucina.pools" . | fromJsonArray -}}
{{- range $in := $pool.instanceNames -}}
{{- range $runner := $pool.runners -}}
{{- $props := list -}}
{{- range $k := keys $runner.properties | sortAlpha -}}
{{- $props = append $props (dict "name" $k "value" (index $runner.properties $k)) -}}
{{- end -}}
{{- $key := printf "%s|%s" $in (toJson $props) -}}
{{- $q := index $queues $key | default (dict "instanceNamePrefix" $in "platform" (dict "properties" $props) "sizeClasses" (list)) -}}
{{- $_ := set $q "sizeClasses" (append $q.sizeClasses (printf "%010d" (int64 $pool.sizeClass))) -}}
{{- $_ := set $queues $key $q -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $out := list -}}
{{- range $key := keys $queues | sortAlpha -}}
{{- $q := index $queues $key -}}
{{- $sizes := list -}}
{{- range $q.sizeClasses | uniq | sortAlpha -}}
{{- /* zero-padded decimal strings sort numerically; atoi is base 10 (int64 would read octal) */ -}}
{{- $sizes = append $sizes (atoi .) -}}
{{- end -}}
{{- $out = append $out (dict "instanceNamePrefix" $q.instanceNamePrefix "platform" $q.platform "sizeClasses" $sizes) -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}
