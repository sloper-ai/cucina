{{/* SPDX-License-Identifier: FSL-1.1-ALv2 */}}
{{/*
Naming, labels, images, size profiles and quantity helpers shared by every template.
*/}}

{{- define "cucina.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cucina.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "cucina.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels. Usage: include "cucina.labels" (dict "ctx" $ "component" "frontend") */}}
{{- define "cucina.labels" -}}
helm.sh/chart: {{ include "cucina.chart" .ctx }}
app.kubernetes.io/name: {{ include "cucina.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
app.kubernetes.io/part-of: cucina
{{- with .component }}
app.kubernetes.io/component: {{ . }}
{{- end }}
{{- with .ctx.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* Selector labels. Usage: include "cucina.selectorLabels" (dict "ctx" $ "component" "frontend") */}}
{{- define "cucina.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cucina.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* Image reference: repository@digest when a digest is set, else repository:tag. */}}
{{- define "cucina.image" -}}
{{- if .digest -}}
{{- printf "%s@%s" .repository .digest -}}
{{- else -}}
{{- printf "%s:%s" .repository .tag -}}
{{- end -}}
{{- end -}}

{{- define "cucina.controllerImage" -}}
{{- include "cucina.image" .Values.images.controller -}}
{{- end -}}

{{- define "cucina.stsImage" -}}
{{- if .Values.images.sts.repository -}}
{{- include "cucina.image" .Values.images.sts -}}
{{- else -}}
{{- include "cucina.image" .Values.images.controller -}}
{{- end -}}
{{- end -}}

{{/* Optional blocks below print nothing when empty; call them as
     {{- with include "…" . }}{{ . | nindent N }}{{- end }} to avoid blank lines. */}}
{{- define "cucina.imagePullSecrets" -}}
{{- with .Values.images.pullSecrets -}}
imagePullSecrets:
{{- range . }}
  - name: {{ . }}
{{- end }}
{{- end -}}
{{- end -}}

{{- define "cucina.clusterId" -}}
{{- default (printf "%s-%s" .Release.Namespace .Release.Name) .Values.clusterId -}}
{{- end -}}

{{/* Fully qualified in-cluster DNS name of a Service. Usage: include "cucina.fqdn" (list $ "name") */}}
{{- define "cucina.fqdn" -}}
{{- $ctx := index . 0 -}}
{{- printf "%s.%s.svc.%s" (index . 1) $ctx.Release.Namespace $ctx.Values.clusterDomain -}}
{{- end -}}

{{/* In-cluster DNS names of a Service (short, namespaced, svc, FQDN), as a YAML list. */}}
{{- define "cucina.serviceDNSNames" -}}
{{- $ctx := index . 0 -}}
{{- $svc := index . 1 -}}
- {{ $svc }}
- {{ printf "%s.%s" $svc $ctx.Release.Namespace }}
- {{ printf "%s.%s.svc" $svc $ctx.Release.Namespace }}
- {{ include "cucina.fqdn" (list $ctx $svc) }}
{{- end -}}

{{/* --- Object names ------------------------------------------------------- */}}
{{- define "cucina.frontend.name" -}}{{ include "cucina.fullname" . }}-frontend{{- end -}}
{{- define "cucina.storage.name" -}}{{ include "cucina.fullname" . }}-storage{{- end -}}
{{- define "cucina.scheduler.name" -}}{{ include "cucina.fullname" . }}-scheduler{{- end -}}
{{- define "cucina.controller.name" -}}{{ include "cucina.fullname" . }}-controller{{- end -}}
{{- define "cucina.sts.name" -}}{{ include "cucina.fullname" . }}-sts{{- end -}}
{{- define "cucina.hooks.name" -}}{{ include "cucina.fullname" . }}-hooks{{- end -}}
{{- define "cucina.client.name" -}}{{ include "cucina.fullname" . }}-client{{- end -}}
{{- define "cucina.worker.name" -}}{{ include "cucina.fullname" . }}-worker{{- end -}}

{{/* Secrets and ConfigMaps created at runtime by the bootstrap hook (never Helm-managed,
     so upgrades never overwrite controller-written content). */}}
{{- define "cucina.caSecret" -}}{{ default (printf "%s-ca" (include "cucina.fullname" .)) .Values.pki.existingCASecret }}{{- end -}}
{{/* Server-certificate Secret of a TLS consumer (pki.CertSpec, docs/security.md):
     "frontend" (client listener) and "sts" belong to the public group, "frontend-workers"
     (worker listener), "scheduler" and "controller" to the internal group. With
     source=existingSecret every consumer of the group mounts that one Secret.
     Usage: include "cucina.tlsSecret" (list $ "scheduler") */}}
{{- define "cucina.tlsGroup" -}}
{{- if has . (list "frontend" "sts") -}}public{{- else -}}internal{{- end -}}
{{- end -}}
{{- define "cucina.tlsSecret" -}}
{{- $ctx := index . 0 -}}
{{- $name := index . 1 -}}
{{- $src := index $ctx.Values.tls (include "cucina.tlsGroup" $name) -}}
{{- if eq $src.source "existingSecret" -}}{{ $src.existingSecret }}{{- else -}}{{ include "cucina.fullname" $ctx }}-tls-{{ $name }}{{- end -}}
{{- end -}}
{{- define "cucina.controllerClientSecret" -}}{{ include "cucina.fullname" . }}-controller-client{{- end -}}
{{- define "cucina.jwksConfigMap" -}}{{ include "cucina.fullname" . }}-jwks{{- end -}}
{{- define "cucina.denyListConfigMap" -}}{{ include "cucina.fullname" . }}-denylist{{- end -}}
{{- define "cucina.signingKeySecret" -}}{{ include "cucina.fullname" . }}-signing-keys{{- end -}}
{{- define "cucina.breakGlassSecret" -}}{{ include "cucina.fullname" . }}-break-glass{{- end -}}
{{- define "cucina.serviceKeysSecret" -}}{{ include "cucina.fullname" . }}-service-keys{{- end -}}
{{- define "cucina.registrySecret" -}}{{ include "cucina.fullname" . }}-registry{{- end -}}

{{/* --- Size profiles -------------------------------------------------------
Effective settings of one component: the size profile (files/profiles.yaml)
overridden by non-empty values under .Values.<component>.
Usage: $eff := include "cucina.effective" (list $ "frontend") | fromYaml */}}
{{- define "cucina.effective" -}}
{{- $ctx := index . 0 -}}
{{- $component := index . 1 -}}
{{- $profiles := $ctx.Files.Get "files/profiles.yaml" | fromYaml -}}
{{- $profile := index $profiles $ctx.Values.sizeProfile | default dict -}}
{{- $base := deepCopy (index $profile $component | default dict) -}}
{{- $user := deepCopy (index $ctx.Values $component | default dict) -}}
{{- toYaml (mergeOverwrite $base $user) -}}
{{- end -}}

{{/* --- Quantities ----------------------------------------------------------
Bytes of a Kubernetes-style quantity: plain integer or Ki/Mi/Gi/Ti suffix. */}}
{{- define "cucina.bytes" -}}
{{- $q := toString . -}}
{{- $units := dict "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776 -}}
{{- $n := 0 -}}
{{- $matched := false -}}
{{- range $suffix, $mul := $units -}}
{{- if hasSuffix $suffix $q -}}
{{- $n = mul (atoi (trimSuffix $suffix $q)) $mul -}}
{{- $matched = true -}}
{{- end -}}
{{- end -}}
{{- if not $matched -}}
{{- if not (regexMatch "^[0-9]+$" $q) -}}
{{- fail (printf "invalid byte quantity %q (use an integer or a Ki/Mi/Gi/Ti suffix)" $q) -}}
{{- end -}}
{{- $n = atoi $q -}}
{{- end -}}
{{- $n -}}
{{- end -}}

{{/* Seconds of a duration like 300s, 5m, 6h or 7d. */}}
{{- define "cucina.seconds" -}}
{{- $d := toString . -}}
{{- if not (regexMatch "^[0-9]+(s|m|h|d)$" $d) -}}
{{- fail (printf "invalid duration %q (use <n>s, <n>m, <n>h or <n>d)" $d) -}}
{{- end -}}
{{- $n := atoi (regexFind "^[0-9]+" $d) -}}
{{- $unit := regexFind "[smhd]$" $d -}}
{{- mul $n (get (dict "s" 1 "m" 60 "h" 3600 "d" 86400) $unit) -}}
{{- end -}}

{{/* GOMEMLIMIT for a container memory limit: 85% of the limit, in MiB. */}}
{{- define "cucina.gomemlimit" -}}
{{- $bytes := include "cucina.bytes" . | int64 -}}
{{- printf "%dMiB" (div (mul $bytes 85) (mul 100 1048576)) -}}
{{- end -}}

{{/* Container env for a Go binary with a memory limit (GOMEMLIMIT). Arg: resources dict. */}}
{{- define "cucina.goEnv" -}}
{{- with .limits -}}
{{- with .memory -}}
- name: GOMEMLIMIT
  value: {{ include "cucina.gomemlimit" . | quote }}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* --- Security contexts --------------------------------------------------- */}}
{{- define "cucina.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
fsGroupChangePolicy: OnRootMismatch
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "cucina.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: ["ALL"]
{{- end -}}

{{/* --- Endpoints ----------------------------------------------------------- */}}
{{/* Issuer URL of Cucina JWTs (no trailing slash). */}}
{{- define "cucina.stsUrl" -}}
{{- if .Values.endpoints.sts.url -}}
{{- trimSuffix "/" .Values.endpoints.sts.url -}}
{{- else if .Values.endpoints.client.host -}}
{{- printf "https://%s:%d" .Values.endpoints.client.host (int .Values.endpoints.sts.port) -}}
{{- else -}}
{{- printf "https://%s:%d" (include "cucina.fqdn" (list . (include "cucina.sts.name" .))) 8443 -}}
{{- end -}}
{{- end -}}

{{/* Hosts of the endpoints, falling back to the in-cluster Service names (usable only
     in-cluster, e.g. for kind tests; real installs set endpoints.*.host). */}}
{{- define "cucina.clientHost" -}}
{{- default (include "cucina.fqdn" (list . (include "cucina.client.name" .))) .Values.endpoints.client.host -}}
{{- end -}}
{{- define "cucina.managementHost" -}}
{{- default (default (include "cucina.fqdn" (list . (printf "%s-api-management" (include "cucina.fullname" .)))) .Values.endpoints.client.host) .Values.endpoints.management.host -}}
{{- end -}}
{{- define "cucina.workerStorageHost" -}}
{{- $w := .Values.endpoints.worker -}}
{{- default (default (include "cucina.fqdn" (list . (printf "%s-storage" (include "cucina.worker.name" .)))) $w.host) $w.storageHost -}}
{{- end -}}
{{- define "cucina.workerSchedulerHost" -}}
{{- $w := .Values.endpoints.worker -}}
{{- default (default (include "cucina.fqdn" (list . (printf "%s-scheduler" (include "cucina.worker.name" .)))) $w.host) $w.schedulerHost -}}
{{- end -}}
{{- define "cucina.workerEnrollmentHost" -}}
{{- $w := .Values.endpoints.worker -}}
{{- default (default (include "cucina.fqdn" (list . (printf "%s-controller" (include "cucina.worker.name" .)))) $w.host) $w.enrollmentHost -}}
{{- end -}}
{{- define "cucina.workerServerName" -}}
{{- default (include "cucina.workerStorageHost" .) .Values.endpoints.worker.serverName -}}
{{- end -}}
{{- define "cucina.hostsHost" -}}
{{- default (include "cucina.workerEnrollmentHost" .) .Values.endpoints.hosts.host -}}
{{- end -}}

{{/* Is the argument an IPv4/IPv6 literal? Returns "true" or "". */}}
{{- define "cucina.isIP" -}}
{{- if or (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$" .) (contains ":" .) -}}true{{- end -}}
{{- end -}}

{{/* The client listener terminates TLS itself unless an Ingress/Gateway does. */}}
{{- define "cucina.clientTLSAtFrontend" -}}
{{- if not (has .Values.exposure.client.type (list "Ingress" "Gateway")) -}}true{{- end -}}
{{- end -}}

{{/* Targets of the in-cluster canaries (`helm test`, the controller's 5-minute loop):
     the in-cluster Services, unless the public certificate cannot carry their names
     (cert-manager without includeServiceNames, e.g. ACME); then the public names, which
     Pods may not resolve or reach (hairpin). The canary rebases the STS URLs that
     discovery advertises onto `sts`. Returns YAML {inCluster, endpoint, sts}. */}}
{{- define "cucina.canaryTargets" -}}
{{- $ports := include "cucina.ports" . | fromYaml -}}
{{- if or (ne .Values.tls.public.source "certManager") .Values.tls.public.certManager.includeServiceNames -}}
{{- $scheme := ternary "grpcs" "grpc" (eq (include "cucina.clientTLSAtFrontend" .) "true") -}}
inCluster: true
endpoint: {{ printf "%s://%s:%d" $scheme (include "cucina.fqdn" (list . (include "cucina.frontend.name" .))) (int $ports.frontendClient) | quote }}
sts: {{ printf "https://%s:%d" (include "cucina.fqdn" (list . (include "cucina.sts.name" .))) (int $ports.controllerSTS) | quote }}
{{- else -}}
inCluster: false
endpoint: {{ printf "grpcs://%s:%d" (include "cucina.clientHost" .) (int .Values.endpoints.client.port) | quote }}
sts: {{ include "cucina.stsUrl" . | quote }}
{{- end -}}
{{- end -}}

{{/* --- Fixed container ports ------------------------------------------------- */}}
{{- define "cucina.ports" -}}
frontendClient: 8980
frontendWorker: 8981
storage: 8981
schedulerClient: 8982
schedulerWorker: 8983
schedulerBuildQueueState: 8984
diagnostics: 9980
controllerProbes: 8081
controllerMetrics: 9090
controllerSTS: 8443
controllerManagement: 8444
controllerEnrollment: 8445
controllerHost: 8446
{{- end -}}

{{/* --- Mount paths shared between the pod specs and the rendered configs -------- */}}
{{- define "cucina.paths" -}}
config: /config
caBundle: /cucina/ca/ca.crt
tlsClients: /cucina/tls/clients
tlsWorkers: /cucina/tls/workers
tlsServer: /cucina/tls/server
controllerClient: /cucina/tls/controller-client
jwks: /cucina/jwks/jwks.json
denyList: /cucina/denylist/denylist.json
storageData: /cucina/storage
storageMeta: /cucina/meta
storageState: /cucina/state
casDevice: /dev/cucina-cas
configDir: /etc/cucina
controllerConfig: /etc/cucina/controller.json
platforms: /etc/cucina/platforms.json
certs: /etc/cucina/certs.json
{{- end -}}
