{{/*
Expand the name of the chart.
*/}}
{{- define "cockroachdb.name" -}}
{{- default .Chart.Name .Values.k8s.nameOverride | trunc 56 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "cockroachdb.fullname" -}}
{{- if .Values.k8s.fullnameOverride -}}
    {{- .Values.k8s.fullnameOverride | trunc 56 | trimSuffix "-" -}}
{{- else -}}
    {{- $name := default .Chart.Name .Values.k8s.nameOverride -}}
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
{{- if .Values.k8s.fullnameOverride -}}
    {{- printf "%s-%s" .Values.k8s.fullnameOverride .Release.Namespace | trunc 56 | trimSuffix "-" -}}
{{- else -}}
    {{- $name := default .Chart.Name .Values.k8s.nameOverride -}}
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
{{- if .Values.cockroachdb.crdbCluster.rbac.serviceAccount.create -}}
    {{- default (include "cockroachdb.fullname" .) .Values.cockroachdb.crdbCluster.rbac.serviceAccount.name -}}
{{- else -}}
    {{- default "default" .Values.cockroachdb.crdbCluster.rbac.serviceAccount.name -}}
{{- end -}}
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
  {{- if .Values.cockroachdb.tls.selfSigner.minimumCertDuration -}}
    {{- print (.Values.cockroachdb.tls.selfSigner.minimumCertDuration | trimSuffix "h") -}}
  {{- else }}
    {{- $minCertDuration := min (sub (.Values.cockroachdb.tls.selfSigner.clientCertDuration | trimSuffix "h" ) (.Values.cockroachdb.tls.selfSigner.clientCertExpiryWindow | trimSuffix "h")) (sub (.Values.cockroachdb.tls.selfSigner.nodeCertDuration | trimSuffix "h") (.Values.cockroachdb.tls.selfSigner.nodeCertExpiryWindow | trimSuffix "h")) -}}
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
{{- $tempHours := sub (.Values.cockroachdb.tls.selfSigner.caCertDuration | trimSuffix "h") (.Values.cockroachdb.tls.selfSigner.caCertExpiryWindow | trimSuffix "h") -}}
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
{{- if .Values.cockroachdb.tls.selfSigner.caProvided -}}
{{- if eq "" .Values.cockroachdb.tls.selfSigner.caSecret -}}
    {{ fail "CA secret can't be empty if caProvided is set to true" }}
{{- else -}}
    {{- if not (lookup "v1" "Secret" .Release.Namespace .Values.cockroachdb.tls.selfSigner.caSecret) }}
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
{{- if not .Values.cockroachdb.tls.selfSigner.caProvided -}}
{{- if or (not .Values.cockroachdb.tls.selfSigner.caCertDuration) (not .Values.cockroachdb.tls.selfSigner.caCertExpiryWindow) }}
  {{ fail "CA cert duration or CA cert expiry window can not be empty" }}
{{- else }}
{{- if gt (int64 (include "selfcerts.minimumCertDuration" .)) (int64 (.Values.cockroachdb.tls.selfSigner.caCertExpiryWindow | trimSuffix "h")) -}}
  {{ fail "CA cert expiration window should not be less than minimum Cert duration" }}
{{- end -}}
{{- if gt (int64 (include "selfcerts.minimumCertDuration" .)) (sub (.Values.cockroachdb.tls.selfSigner.caCertDuration | trimSuffix "h") (.Values.cockroachdb.tls.selfSigner.caCertExpiryWindow | trimSuffix "h")) -}}
  {{ fail "CA cert Duration minus CA cert expiration window should not be less than minimum Cert duration" }}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Validate that if clientCertDuration must not be empty and it must be greater than minimumCertDuration.
*/}}
{{- define "cockroachdb.tls.certs.selfSigner.clientCertValidation" -}}
{{- if or (not .Values.cockroachdb.tls.selfSigner.clientCertDuration) (not .Values.cockroachdb.tls.selfSigner.clientCertExpiryWindow) }}
  {{ fail "Client cert duration can not be empty" }}
{{- else }}
{{- if lt (sub (.Values.cockroachdb.tls.selfSigner.clientCertDuration | trimSuffix "h") (.Values.cockroachdb.tls.selfSigner.clientCertExpiryWindow | trimSuffix "h")) (int64 (include "selfcerts.minimumCertDuration" .)) }}
   {{ fail "Client cert duration minus client cert expiry window should not be less than minimum Cert duration" }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Validate that nodeCertDuration must not be empty and nodeCertDuration minus nodeCertExpiryWindow must be greater than minimumCertDuration.
*/}}
{{- define "cockroachdb.tls.certs.selfSigner.nodeCertValidation" -}}
{{- if or (not .Values.cockroachdb.tls.selfSigner.nodeCertDuration) (not .Values.cockroachdb.tls.selfSigner.nodeCertExpiryWindow) }}
  {{ fail "Node cert duration can not be empty" }}
{{- else }}
{{- if lt (sub (.Values.cockroachdb.tls.selfSigner.nodeCertDuration | trimSuffix "h") (.Values.cockroachdb.tls.selfSigner.nodeCertExpiryWindow | trimSuffix "h")) (int64 (include "selfcerts.minimumCertDuration" .))}}
   {{ fail "Node cert duration minus node cert expiry window should not be less than minimum Cert duration" }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Validate that if user enabled tls, then only one of self-signed certificates, certificate manager, or user provided certificates is enabled
*/}}
{{- define "cockroachdb.tlsValidation" -}}
{{- if .Values.cockroachdb.tls.enabled -}}
{{- $selfSigner := .Values.cockroachdb.tls.selfSigner.enabled }}
{{- $certManager := .Values.cockroachdb.tls.certManager.enabled }}
{{- $userProvided := .Values.cockroachdb.tls.externalCertificates.enabled }}
{{- $enabledCount := 0 }}
{{- if $selfSigner }}{{ $enabledCount = add1 $enabledCount }}{{ end }}
{{- if $certManager }}{{ $enabledCount = add1 $enabledCount }}{{ end }}
{{- if $userProvided }}{{ $enabledCount = add1 $enabledCount }}{{ end }}
{{- if ne $enabledCount 1 }}
    {{ fail "Exactly one of self-signed certificates, certificate manager, or user provided certificates must be enabled when TLS is enabled" }}
{{- end }}
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
{{- if ne "cockroachdb/cockroach" .Values.cockroachdb.crdbCluster.image.repository -}}
    {{ print true }}
{{- else -}}
{{- if semverCompare ">=22.1.2" .Values.cockroachdb.crdbCluster.image.tag -}}
    {{ print true }}
{{- else -}}
{{- if semverCompare ">=21.2.13, <22.1.0" .Values.cockroachdb.crdbCluster.image.tag -}}
    {{ print true }}
{{- else -}}
    {{ print false }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Validate the WAL failover configuration.
*/}}
{{- define "cockroachdb.conf.wal-failover.validation" -}}
  {{- with index .Values.cockroachdb.crdbCluster.conf `wal-failover` -}}
    {{- if not (mustHas .value (list "" "disabled" "among-stores")) -}}
        {{- if not (hasPrefix "path=" (.value | toString)) -}}
            {{ fail "Invalid WAL failover configuration value. Expected either of '', 'disabled', 'among-stores' or 'path=<path>'" }}
        {{- end -}}
    {{- end -}}
    {{- if eq .value "among-stores" -}}
      {{- if or (not $.Values.conf.store.enabled) (eq (int $.Values.conf.store.count) 1) -}}
        {{ fail "WAL failover among stores requires store enabled with count greater than 1" }}
      {{- end -}}
    {{- end -}}
    {{- if hasPrefix "path=" (.value | toString) -}}
      {{- if not .persistentVolume.enabled -}}
        {{ fail "WAL failover to a side disk requires a persistent volume" }}
      {{- end -}}
      {{- if and (not (hasPrefix (printf "/cockroach/%s" .persistentVolume.path) (trimPrefix "path=" .value))) (not (hasPrefix .persistentVolume.path (trimPrefix "path=" .value))) -}}
        {{ fail "WAL failover to a side disk requires a path to the mounted persistent volume" }}
      {{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{/*
Construct the GODEBUG env var value (looks like: GODEBUG="foo=bar,baz=quux"; default: "disablethp=1")
*/}}
{{- define "godebugList" -}}
{{- $godebugList := list -}}
{{- range $key, $value := .Values.cockroachdb.crdbCluster.godebug }}
  {{- $godebugList = append $godebugList (printf "%s=%s" $key $value) -}}
{{- end }}
{{- join "," $godebugList -}}
{{- end }}

{{/* Common labels that are applied to all managed objects. */}}
{{- define "cluster.labels" -}}
helm.sh/chart: {{ include "cockroachdb.chart" . }}
{{ include "cluster.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
  Selector labels defines the set of labels that can be used as selectors for
  CockroachDB nodes.
*/}}
{{- define "cluster.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cockroachdb.clusterfullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
