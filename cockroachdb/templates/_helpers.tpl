{{/*
Expand the name of the chart.
*/}}
{{- define "cockroachdb.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 56 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "cockroachdb.fullname" -}}
{{- if .Values.fullnameOverride -}}
    {{- .Values.fullnameOverride | trunc 56 | trimSuffix "-" -}}
{{- else -}}
    {{- $name := default .Chart.Name .Values.nameOverride -}}
    {{- if contains $name .Release.Name -}}
        {{- .Release.Name | trunc 56 | trimSuffix "-" -}}
    {{- else -}}
        {{- printf "%s-%s" .Release.Name $name | trunc 56 | trimSuffix "-" -}}
    {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Create a default fully qualified app name for cluster scope resource.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name with release namespace appended at the end.
*/}}
{{- define "cockroachdb.clusterfullname" -}}
{{- if .Values.fullnameOverride -}}
    {{- printf "%s-%s" .Values.fullnameOverride .Release.Namespace | trunc 56 | trimSuffix "-" -}}
{{- else -}}
    {{- $name := default .Chart.Name .Values.nameOverride -}}
    {{- if contains $name .Release.Name -}}
        {{- printf "%s-%s" .Release.Name .Release.Namespace | trunc 56 | trimSuffix "-" -}}
    {{- else -}}
        {{- printf "%s-%s-%s" .Release.Name $name .Release.Namespace | trunc 56 | trimSuffix "-" -}}
    {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "cockroachdb.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 56 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create the name of the ServiceAccount to use.
*/}}
{{- define "cockroachdb.serviceAccount.name" -}}
{{- if .Values.statefulset.serviceAccount.create -}}
    {{- default (include "cockroachdb.fullname" .) .Values.statefulset.serviceAccount.name -}}
{{- else -}}
    {{- default "default" .Values.statefulset.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Return the appropriate apiVersion for NetworkPolicy.
*/}}
{{- define "cockroachdb.networkPolicy.apiVersion" -}}
{{- if semverCompare ">=1.4-0, <=1.7-0" .Capabilities.KubeVersion.Version -}}
    {{- print "extensions/v1beta1" -}}
{{- else if semverCompare "^1.7-0" .Capabilities.KubeVersion.Version -}}
    {{- print "networking.k8s.io/v1" -}}
{{- end -}}
{{- end -}}

{{/*
Return the appropriate apiVersion for StatefulSets
*/}}
{{- define "cockroachdb.statefulset.apiVersion" -}}
{{- if semverCompare "<1.12-0" .Capabilities.KubeVersion.Version -}}
    {{- print "apps/v1beta1" -}}
{{- else -}}
    {{- print "apps/v1" -}}
{{- end -}}
{{- end -}}

{{/*
Return CockroachDB store expression
*/}}
{{- define "cockroachdb.conf.store" -}}
{{- $isInMemory := eq (.Values.conf.store.type | toString) "mem" -}}
{{- $persistentSize := empty .Values.conf.store.size | ternary .Values.storage.persistentVolume.size .Values.conf.store.size -}}

{{- $store := dict -}}
{{- $_ := set $store "type" ($isInMemory | ternary "type=mem" "") -}}
{{- $_ := set $store "path" ($isInMemory | ternary "" (print "path=" .Values.conf.path)) -}}
{{- $_ := set $store "size" (print "size=" ($isInMemory | ternary .Values.conf.store.size $persistentSize)) -}}
{{- $_ := set $store "attrs" (empty .Values.conf.store.attrs | ternary "" (print "attrs=" .Values.conf.store.attrs)) -}}

{{ compact (values $store) | join "," }}
{{- end -}}

{{/*
Define the default values for the certificate selfSigner inputs
*/}}
{{- define "selfcerts.fullname" -}}
  {{- printf "%s-%s" (include "cockroachdb.fullname" .) "self-signer" | trunc 56 | trimSuffix "-" -}}
{{- end -}}

{{- define "rotatecerts.fullname" -}}
  {{- printf "%s-%s" (include "cockroachdb.fullname" .) "rotate-self-signer" | trunc 56 | trimSuffix "-" -}}
{{- end -}}

{{- define "selfcerts.minimumCertDuration" -}}
  {{- if .Values.tls.certs.selfSigner.minimumCertDuration -}}
    {{- print (.Values.tls.certs.selfSigner.minimumCertDuration | trimSuffix "h") -}}
  {{- else }}
    {{- $minCertDuration := min (sub (.Values.tls.certs.selfSigner.clientCertDuration | trimSuffix "h" ) (.Values.tls.certs.selfSigner.clientCertExpiryWindow | trimSuffix "h")) (sub (.Values.tls.certs.selfSigner.nodeCertDuration | trimSuffix "h") (.Values.tls.certs.selfSigner.nodeCertExpiryWindow | trimSuffix "h")) -}}
    {{- print $minCertDuration -}}
  {{- end }}
{{- end -}}

{{/*
Define the cron schedules for certificate rotate jobs and converting from hours to valid cron string.
We assume that each month has 31 days, hence the cron job may run few days earlier in a year. In a cron schedule,
we can not set a cron of more than a year, hence we try to run the cron in such a way that the cron run comes to
as close possible to the expiry window. However, it is possible that cron may run earlier than the expiry window.
*/}}
{{- define "selfcerts.caRotateSchedule" -}}
{{- $tempHours := sub (.Values.tls.certs.selfSigner.caCertDuration | trimSuffix "h") (.Values.tls.certs.selfSigner.caCertExpiryWindow | trimSuffix "h") -}}
{{- $days := "*" -}}
{{- $months := "*" -}}
{{- $hours := mod $tempHours 24 -}}
{{- if not (eq $hours $tempHours) -}}
{{- $tempDays := div $tempHours 24 -}}
{{- $days = mod $tempDays 31 -}}
{{- if not (eq $days $tempDays) -}}
{{- $days = add $days 1 -}}
{{- $tempMonths := div $tempDays 31 -}}
{{- $months = mod $tempMonths 12 -}}
{{- if not (eq $months $tempMonths) -}}
{{- $months = add $months 1 -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if ne (toString $months) "*" -}}
{{- $months = printf "*/%s" (toString $months) -}}
{{- else -}}
{{- if ne (toString $days) "*" -}}
{{- $days = printf "*/%s" (toString $days) -}}
{{- else -}}
{{- if ne $hours 0 -}}
{{- $hours = printf "*/%s" (toString $hours) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- printf "0 %s %s %s *" (toString $hours) (toString $days) (toString $months) -}}
{{- end -}}

{{- define "selfcerts.clientRotateSchedule" -}}
{{- $tempHours := int64 (include "selfcerts.minimumCertDuration" .) -}}
{{- $days := "*" -}}
{{- $months := "*" -}}
{{- $hours := mod $tempHours 24 -}}
{{- if not (eq $hours $tempHours) -}}
{{- $tempDays := div $tempHours 24 -}}
{{- $days = mod $tempDays 31 -}}
{{- if not (eq $days $tempDays) -}}
{{- $days = add $days 1 -}}
{{- $tempMonths := div $tempDays 31 -}}
{{- $months = mod $tempMonths 12 -}}
{{- if not (eq $months $tempMonths) -}}
{{- $months = add $months 1 -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if ne (toString $months) "*" -}}
{{- $months = printf "*/%s" (toString $months) -}}
{{- else -}}
{{- if ne (toString $days) "*" -}}
{{- $days = printf "*/%s" (toString $days) -}}
{{- else -}}
{{- if ne $hours 0 -}}
{{- $hours = printf "*/%s" (toString $hours) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- printf "0 %s %s %s *" (toString $hours) (toString $days) (toString $months) -}}
{{- end -}}

{{/*
Define the appropriate validations for the certificate selfSigner inputs
*/}}

{{/*
Validate that if caProvided is true, then the caSecret must not be empty and secret must be present in the namespace.
*/}}
{{- define "cockroachdb.tls.certs.selfSigner.caProvidedValidation" -}}
{{- if .Values.tls.certs.selfSigner.caProvided -}}
{{- if eq "" .Values.tls.certs.selfSigner.caSecret -}}
    {{ fail "CA secret can't be empty if caProvided is set to true" }}
{{- else -}}
    {{- if not (lookup "v1" "Secret" .Release.Namespace .Values.tls.certs.selfSigner.caSecret) }}
        {{ fail "CA secret is not present in the release namespace" }}
    {{- end }}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Validate that if caCertDuration or caCertExpiryWindow must not be empty and caCertExpiryWindow must be greater than
minimumCertDuration.
*/}}
{{- define "cockroachdb.tls.certs.selfSigner.caCertValidation" -}}
{{- if not .Values.tls.certs.selfSigner.caProvided -}}
{{- if or (not .Values.tls.certs.selfSigner.caCertDuration) (not .Values.tls.certs.selfSigner.caCertExpiryWindow) }}
  {{ fail "CA cert duration or CA cert expiry window can not be empty" }}
{{- else }}
{{- if gt (int64 (include "selfcerts.minimumCertDuration" .)) (int64 (.Values.tls.certs.selfSigner.caCertExpiryWindow | trimSuffix "h")) -}}
  {{ fail "CA cert expiration window should not be less than minimum Cert duration" }}
{{- end -}}
{{- if gt (int64 (include "selfcerts.minimumCertDuration" .)) (sub (.Values.tls.certs.selfSigner.caCertDuration | trimSuffix "h") (.Values.tls.certs.selfSigner.caCertExpiryWindow | trimSuffix "h")) -}}
  {{ fail "CA cert Duration minus CA cert expiration window should not be less than minimum Cert duration" }}
{{- end -}}
{{- end -}}
{{- end }}
{{- end -}}

{{/*
Validate that if clientCertDuration must not be empty and it must be greater than minimumCertDuration.
*/}}
{{- define "cockroachdb.tls.certs.selfSigner.clientCertValidation" -}}
{{- if or (not .Values.tls.certs.selfSigner.clientCertDuration) (not .Values.tls.certs.selfSigner.clientCertExpiryWindow) }}
  {{ fail "Client cert duration can not be empty" }}
{{- else }}
{{- if lt (sub (.Values.tls.certs.selfSigner.clientCertDuration | trimSuffix "h") (.Values.tls.certs.selfSigner.clientCertExpiryWindow | trimSuffix "h")) (int64 (include "selfcerts.minimumCertDuration" .)) }}
   {{ fail "Client cert duration minus client cert expiry window should not be less than minimum Cert duration" }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Validate that nodeCertDuration must not be empty and nodeCertDuration minus nodeCertExpiryWindow must be greater than minimumCertDuration.
*/}}
{{- define "cockroachdb.tls.certs.selfSigner.nodeCertValidation" -}}
{{- if or (not .Values.tls.certs.selfSigner.nodeCertDuration) (not .Values.tls.certs.selfSigner.nodeCertExpiryWindow) }}
  {{ fail "Node cert duration can not be empty" }}
{{- else }}
{{- if lt (sub (.Values.tls.certs.selfSigner.nodeCertDuration | trimSuffix "h") (.Values.tls.certs.selfSigner.nodeCertExpiryWindow | trimSuffix "h")) (int64 (include "selfcerts.minimumCertDuration" .))}}
   {{ fail "Node cert duration minus node cert expiry window should not be less than minimum Cert duration" }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Validate that if user enabled tls, then either self-signed certificates or certificate manager is enabled
*/}}
{{- define "cockroachdb.tlsValidation" -}}
{{- if .Values.tls.enabled -}}
{{- if and .Values.tls.certs.selfSigner.enabled .Values.tls.certs.certManager -}}
    {{ fail "Can not enable the self signed certificates and certificate manager at the same time" }}
{{- end -}}
{{- if and (not .Values.tls.certs.selfSigner.enabled) (not .Values.tls.certs.certManager) -}}
    {{- if not .Values.tls.certs.provided -}}
        {{ fail "You have to enable either self signed certificates or certificate manager, if you have enabled tls" }}
    {{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}


{{- define "cockroachdb.tls.certs.selfSigner.validation" -}}
{{ include "cockroachdb.tls.certs.selfSigner.caProvidedValidation" . }}
{{ include "cockroachdb.tls.certs.selfSigner.caCertValidation" . }}
{{ include "cockroachdb.tls.certs.selfSigner.clientCertValidation" . }}
{{ include "cockroachdb.tls.certs.selfSigner.nodeCertValidation" . }}
{{- end -}}

{{- define "cockroachdb.securityContext.versionValidation" }}
{{- /* Allow using `securityContext` for custom images. */}}
{{- if ne "cockroachdb/cockroach" .Values.image.repository -}}
    {{ print true }}
{{- else -}}
{{- if semverCompare ">=22.1.2" .Values.image.tag -}}
    {{ print true }}
{{- else -}}
{{- if semverCompare ">=21.2.13, <22.1.0" .Values.image.tag -}}
    {{ print true }}
{{- else -}}
    {{ print false }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
