{{/* SPDX-License-Identifier: FSL-1.1-ALv2 */}}
{{/* All PrometheusRule groups (monitoring/rules.yaml). */}}
{{- define "cucina.rules" -}}
{{- $groups := list -}}
{{- range $file := list "files/rules/buildbarn-recording.yaml" "files/rules/cucina.yaml" "files/rules/slo.yaml" -}}
{{- $raw := $.Files.Get $file -}}
{{- /* Only cucina.yaml is a template; the others contain Prometheus' own {{ }} syntax. */ -}}
{{- if eq $file "files/rules/cucina.yaml" -}}
{{- $raw = tpl $raw $ -}}
{{- end -}}
{{- $content := $raw | fromYaml -}}
{{- if $content.Error -}}{{- fail (printf "%s: %s" $file $content.Error) -}}{{- end -}}
{{- $groups = concat $groups ($content.groups | default list) -}}
{{- end -}}
{{- toYaml (dict "groups" $groups) -}}
{{- end -}}
