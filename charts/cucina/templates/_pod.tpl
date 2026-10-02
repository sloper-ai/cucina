{{/* SPDX-License-Identifier: FSL-1.1-ALv2 */}}
{{/* Pod-spec fragments shared by the Buildbarn components. */}}

{{/* Probes on Buildbarn's diagnostics server: /-/healthy turns 200 once the process started (R-CP-6). */}}
{{- define "cucina.bb.probes" -}}
startupProbe:
  httpGet:
    path: /-/healthy
    port: diagnostics
  periodSeconds: 5
  failureThreshold: 120
readinessProbe:
  httpGet:
    path: /-/healthy
    port: diagnostics
  periodSeconds: 10
  failureThreshold: 3
livenessProbe:
  httpGet:
    path: /-/healthy
    port: diagnostics
  periodSeconds: 20
  timeoutSeconds: 5
  failureThreshold: 6
{{- end -}}

{{/* Probes of cucina-controller and the STS (config.Listeners.Probes). */}}
{{- define "cucina.controller.probes" -}}
startupProbe:
  httpGet:
    path: /-/healthy
    port: probes
  periodSeconds: 2
  failureThreshold: 60
readinessProbe:
  httpGet:
    path: /-/ready
    port: probes
  periodSeconds: 5
  failureThreshold: 3
livenessProbe:
  httpGet:
    path: /-/healthy
    port: probes
  periodSeconds: 20
  timeoutSeconds: 5
  failureThreshold: 3
{{- end -}}

{{/*
Volumes every authorizing Buildbarn process mounts: the Cucina CA certificate (only
ca.crt, never the key), the JWKS and the deny-list. JWKS and deny-list are written
by the controller at runtime and mounted as directories (never subPath) so updates
propagate (R-AUTH-4/-9).
*/}}
{{- define "cucina.bb.authVolumes" -}}
- name: ca
  secret:
    secretName: {{ include "cucina.caSecret" . }}
    items:
      - key: ca.crt
        path: ca.crt
- name: jwks
  configMap:
    name: {{ include "cucina.jwksConfigMap" . }}
- name: denylist
  configMap:
    name: {{ include "cucina.denyListConfigMap" . }}
{{- end -}}

{{/* Default hostname spread for a component unless the user set constraints. Args: list $ component constraints */}}
{{- define "cucina.topologySpread" -}}
{{- $ctx := index . 0 -}}
{{- $component := index . 1 -}}
{{- $user := index . 2 -}}
topologySpreadConstraints:
{{- if $user }}
  {{- toYaml $user | nindent 2 }}
{{- else }}
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        {{- include "cucina.selectorLabels" (dict "ctx" $ctx "component" $component) | nindent 8 }}
{{- end }}
{{- end -}}

{{/* nodeSelector / tolerations / affinity / priorityClassName from an effective component dict. */}}
{{- define "cucina.scheduling" -}}
{{- $out := list -}}
{{- with .priorityClassName }}{{ $out = append $out (printf "priorityClassName: %s" .) }}{{ end -}}
{{- with .nodeSelector }}{{ $out = append $out (dict "nodeSelector" . | toYaml | trim) }}{{ end -}}
{{- with .tolerations }}{{ $out = append $out (dict "tolerations" . | toYaml | trim) }}{{ end -}}
{{- with .affinity }}{{ $out = append $out (dict "affinity" . | toYaml | trim) }}{{ end -}}
{{- join "\n" $out -}}
{{- end -}}

{{/* A Service exposing an endpoint (exposure/services.yaml). */}}
{{- define "cucina.exposedService" -}}
{{- $ctx := .ctx -}}
{{- $exp := .exposure -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ .name }}
  namespace: {{ $ctx.Release.Namespace }}
  labels:
    {{- include "cucina.labels" (dict "ctx" $ctx "component" .component) | nindent 4 }}
    cucina.sloper.ai/endpoint: {{ .endpoint }}
  {{- with $exp.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  type: {{ $exp.type }}
  {{- if eq $exp.type "LoadBalancer" }}
  {{- with $exp.loadBalancerClass }}
  loadBalancerClass: {{ . }}
  {{- end }}
  {{- with $exp.loadBalancerSourceRanges }}
  loadBalancerSourceRanges:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- end }}
  {{- if has $exp.type (list "LoadBalancer" "NodePort") }}
  externalTrafficPolicy: {{ $exp.externalTrafficPolicy }}
  {{- end }}
  selector:
    {{- include "cucina.selectorLabels" (dict "ctx" $ctx "component" .component) | nindent 4 }}
    {{- with .extraSelector }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  ports:
    {{- range .ports }}
    - name: {{ .name }}
      port: {{ .port }}
      targetPort: {{ .targetPort }}
      protocol: TCP
      {{- if and (eq $exp.type "NodePort") .nodePort }}
      nodePort: {{ .nodePort }}
      {{- else if and (eq $exp.type "LoadBalancer") .nodePort }}
      nodePort: {{ .nodePort }}
      {{- end }}
    {{- end }}
{{- end -}}
