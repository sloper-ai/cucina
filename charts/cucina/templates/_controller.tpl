{{/* SPDX-License-Identifier: FSL-1.1-ALv2 */}}
{{/*
controller.json: exactly internal/config.Controller (strict parse at startup,
unknown fields are an error). bootstrap.json: certificates the pre-install hook
creates when TLS is chart-generated (docs/operations/chart.md §Bootstrap).
*/}}
{{- define "cucina.controller.config" -}}
{{- $ports := include "cucina.ports" . | fromYaml -}}
{{- $paths := include "cucina.paths" . | fromYaml -}}
{{- $eff := include "cucina.effective" (list . "controller") | fromYaml -}}
{{- $c := .Values.controller -}}
{{- $cfg := dict
  "clusterId" (include "cucina.clusterId" .)
  "namespace" .Release.Namespace
  "releaseName" .Release.Name
  "instanceNames" .Values.instanceNames
  "platformsFile" $paths.platforms
  "leaderElection" (dict
    "enabled" (gt (int $eff.replicas) 1)
    "id" (printf "%s-leader" (include "cucina.controller.name" .))
    "leaseDuration" $c.leaderElection.leaseDuration
    "renewDeadline" $c.leaderElection.renewDeadline
    "retryPeriod" $c.leaderElection.retryPeriod)
  "listeners" (dict
    "probes" (printf ":%d" (int $ports.controllerProbes))
    "metrics" (printf ":%d" (int $ports.controllerMetrics))
    "sts" (printf ":%d" (int $ports.controllerSTS))
    "management" (printf ":%d" (int $ports.controllerManagement))
    "enrollment" (printf ":%d" (int $ports.controllerEnrollment))
    "host" (printf ":%d" (int $ports.controllerHost)))
  "tls" (dict
    "certFile" (printf "%s/tls.crt" $paths.tlsServer)
    "keyFile" (printf "%s/tls.key" $paths.tlsServer)
    "caFile" $paths.caBundle)
  "pki" (dict
    "caSecret" (include "cucina.caSecret" .)
    "workerCertTTL" .Values.pki.workerCertTTL
    "hostCertTTL" .Values.pki.hostCertTTL
    "vmCertTTL" .Values.pki.vmCertTTL)
  "scheduler" (dict
    "buildQueueStateAddress" (printf "%s:%d" (include "cucina.fqdn" (list . (include "cucina.scheduler.name" .))) (int $ports.schedulerBuildQueueState))
    "pollInterval" $c.scheduler.pollInterval
    "clientCertFile" (printf "%s/tls.crt" $paths.controllerClient)
    "clientKeyFile" (printf "%s/tls.key" $paths.controllerClient)
    "queueFailAfter" $c.scheduler.queueFailAfter)
  "endpoints" (dict
    "clientEndpoint" (printf "grpcs://%s:%d" (include "cucina.clientHost" .) (int .Values.endpoints.client.port))
    "workerScheduler" (printf "%s:%d" (include "cucina.workerSchedulerHost" .) (int .Values.endpoints.worker.schedulerPort))
    "workerStorage" (printf "%s:%d" (include "cucina.workerStorageHost" .) (int .Values.endpoints.worker.storagePort))
    "workerEnroll" (printf "%s:%d" (include "cucina.workerEnrollmentHost" .) (int .Values.endpoints.worker.enrollmentPort))
    "hostEndpoint" (include "cucina.hostPort" (list (include "cucina.hostsHost" .) .Values.endpoints.hosts.port))
    "hostStorage" (include "cucina.hostPort" (list (include "cucina.hostStorageHost" .) .Values.endpoints.worker.storagePort))
    "hostScheduler" (include "cucina.hostPort" (list (include "cucina.hostSchedulerHost" .) .Values.endpoints.worker.schedulerPort))
    "serverName" (include "cucina.workerServerName" .)
    "stsUrl" (include "cucina.stsUrl" .)
    "managementUrl" (printf "%s:%d" (include "cucina.managementHost" .) (int .Values.endpoints.management.port)))
  "auth" (dict
    "signingKeySecret" (include "cucina.signingKeySecret" .)
    "jwksConfigMap" (include "cucina.jwksConfigMap" .)
    "denyListConfigMap" (include "cucina.denyListConfigMap" .)
    "tokenTTL" .Values.auth.tokenTTL
    "audience" .Values.auth.audience
    "keyRotationPublishLead" .Values.auth.keyRotationPublishLead
    "breakGlassKeySecret" (ternary (include "cucina.breakGlassSecret" .) "" .Values.auth.breakGlass.enabled)
    "serviceKeysSecret" (include "cucina.serviceKeysSecret" .)
    "rateLimitPerMinute" .Values.auth.rateLimitPerMinute)
  "hosts" (dict
    "defaultTokenTtl" $c.hosts.defaultTokenTtl
    "staleAfter" $c.hosts.staleAfter
    "registrySecret" (include "cucina.registrySecret" .))
  "worker" (dict
    "maximumMessageSizeBytes" .Values.buildbarn.maximumMessageSizeBytes
    "metricsPort" $c.worker.metricsPort
    "pushgatewayUrl" (include "cucina.pushgatewayUrl" .)
    "wanCompressionForHosts" $c.worker.wanCompressionForHosts)
  "autoscaler" (dict
    "shadow" $c.autoscaler.shadow
    "runInstancesBurst" $c.autoscaler.runInstancesBurst
    "runInstancesRefillPerSecond" $c.autoscaler.runInstancesRefillPerSecond
    "deadmanIdleLimit" $c.autoscaler.deadmanIdleLimit
    "deadmanUnreachableLimit" $c.autoscaler.deadmanUnreachableLimit
    "deadmanMaxUptime" $c.autoscaler.deadmanMaxUptime
    "iceBackoffMin" $c.autoscaler.iceBackoffMin
    "iceBackoffMax" $c.autoscaler.iceBackoffMax)
  "observability" (dict
    "logLevel" $c.logLevel
    "otlpEndpoint" $c.otlpEndpoint
    "costEnabled" $c.costEnabled) -}}
{{- with .Values.endpoints.sts.aliases -}}
{{- $_ := set $cfg.endpoints "stsAliases" . -}}
{{- end -}}
{{- with .Values.auth.groupLookup -}}
{{- $_ := set $cfg.auth "groupLookup" (dict "serviceAccountSecret" .serviceAccountSecret "cacheTtl" .cacheTtl) -}}
{{- end -}}
{{- if $c.aws.enabled -}}
{{- $aws := dict
  "region" (required "controller.aws.region is required when controller.aws.enabled" $c.aws.region)
  "accountId" (required "controller.aws.accountId is required when controller.aws.enabled" $c.aws.accountId)
  "sweepInterval" $c.aws.sweepInterval -}}
{{- with $c.aws.extraTags }}{{ $_ := set $aws "extraTags" . }}{{ end -}}
{{- with $c.aws.pricingRegionCode }}{{ $_ := set $aws "pricingRegionCode" . }}{{ end -}}
{{- $_ := set $cfg "aws" $aws -}}
{{- end -}}
{{- if $c.registry.enabled -}}
{{- $_ := set $cfg "registry" (dict "host" $c.registry.host "mode" $c.registry.mode) -}}
{{- end -}}
{{- toPrettyJson $cfg -}}
{{- end -}}

{{/* Split names into DNS names and IP addresses. Arg: list of names. */}}
{{- define "cucina.sans" -}}
{{- $dns := list -}}
{{- $ips := list -}}
{{- range . | uniq -}}
{{- if include "cucina.isIP" . -}}
{{- $ips = append $ips . -}}
{{- else if . -}}
{{- $dns = append $dns . -}}
{{- end -}}
{{- end -}}
{{- toJson (dict "dnsNames" $dns "ipAddresses" $ips) -}}
{{- end -}}

{{/*
Every certificate the chart needs, as pki.CertSpec (internal/pki/secrets.go, ADR 0552):
one server certificate per TLS consumer, carrying spiffe://cucina/server/<component>
and the consumer's DNS/IP SANs, plus the controller's BuildQueueState client
certificate. Each spec also carries "group" (public/internal, chart-only; stripped
from certs.json). In-cluster Service names are left out of public certificates issued
by cert-manager unless tls.public.certManager.includeServiceNames (ACME cannot
validate them).
*/}}
{{- define "cucina.certSpecs" -}}
{{- $f := include "cucina.fullname" . -}}
{{- $stsHost := (urlParse (include "cucina.stsUrl" .)).hostname -}}
{{- $stsNames := list $stsHost -}}
{{- range .Values.endpoints.sts.aliases -}}
{{- $stsNames = append $stsNames (urlParse .).hostname -}}
{{- end -}}
{{- $w := .Values.endpoints.worker -}}
{{- $consumers := list
  (dict "name" "frontend" "component" "frontend"
        "names" (concat (list (include "cucina.clientHost" .)) .Values.endpoints.client.extraNames)
        "services" (list (include "cucina.frontend.name" .) (include "cucina.client.name" .)))
  (dict "name" "sts" "component" "sts"
        "names" (concat $stsNames .Values.endpoints.client.extraNames)
        "services" (list (include "cucina.sts.name" .) (printf "%s-api-sts" $f)))
  (dict "name" "frontend-workers" "component" "frontend"
        "names" (concat (list (include "cucina.workerStorageHost" .) (include "cucina.workerServerName" .) (include "cucina.hostStorageHost" .)) $w.extraNames)
        "services" (list (include "cucina.frontend.name" .) (printf "%s-storage" (include "cucina.worker.name" .))))
  (dict "name" "scheduler" "component" "scheduler"
        "names" (concat (list (include "cucina.workerSchedulerHost" .) (include "cucina.workerServerName" .) (include "cucina.hostSchedulerHost" .)) $w.extraNames)
        "services" (list (include "cucina.scheduler.name" .) (printf "%s-scheduler" (include "cucina.worker.name" .))))
  (dict "name" "controller" "component" "controller"
        "names" (concat (list (include "cucina.workerEnrollmentHost" .) (include "cucina.hostsHost" .) (include "cucina.workerServerName" .) (include "cucina.managementHost" .)) $stsNames $w.extraNames .Values.endpoints.client.extraNames)
        "services" (list (include "cucina.controller.name" .) (printf "%s-api-management" $f) (printf "%s-controller" (include "cucina.worker.name" .)))) -}}
{{- $specs := list -}}
{{- range $consumers -}}
{{- $group := include "cucina.tlsGroup" .name -}}
{{- $names := .names -}}
{{- if or (eq $group "internal") (ne $.Values.tls.public.source "certManager") $.Values.tls.public.certManager.includeServiceNames -}}
{{- range $svc := .services -}}
{{- $names = concat $names (include "cucina.serviceDNSNames" (list $ $svc) | fromYamlArray) -}}
{{- end -}}
{{- end -}}
{{- $specs = append $specs (merge (dict
  "secretName" (include "cucina.tlsSecret" (list $ .name))
  "role" "server"
  "component" .component
  "lifetime" $.Values.tls.serverCertificateDuration
  "group" $group) (include "cucina.sans" $names | fromJson)) -}}
{{- end -}}
{{- $specs = append $specs (dict "secretName" (include "cucina.controllerClientSecret" .) "role" "controller" "lifetime" .Values.pki.controllerClientCertTTL "group" "controller") -}}
{{- toJson $specs -}}
{{- end -}}

{{/* certs.json for `cucina-controller bootstrap/controller --certs`: the certificates
     Cucina issues and renews itself (chart-generated groups + the controller client). */}}
{{- define "cucina.certsFile" -}}
{{- $out := list -}}
{{- range include "cucina.certSpecs" . | fromJsonArray -}}
{{- if or (eq .group "controller") (eq (index $.Values.tls .group).source "generated") -}}
{{- $spec := omit . "group" -}}
{{- if not $spec.dnsNames -}}{{- $spec = omit $spec "dnsNames" -}}{{- end -}}
{{- if not $spec.ipAddresses -}}{{- $spec = omit $spec "ipAddresses" -}}{{- end -}}
{{- $out = append $out $spec -}}
{{- end -}}
{{- end -}}
{{- toPrettyJson $out -}}
{{- end -}}

{{- define "cucina.pushgatewayUrl" -}}
{{- if .Values.monitoring.pushgateway.enabled -}}
{{- printf "http://%s:9091" (include "cucina.fqdn" (list . (printf "%s-pushgateway" (include "cucina.fullname" .)))) -}}
{{- else -}}
{{- .Values.controller.worker.pushgatewayUrl -}}
{{- end -}}
{{- end -}}
