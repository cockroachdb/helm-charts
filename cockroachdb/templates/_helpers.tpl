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
Create chart name and version as used by the chart label.
*/}}
{{- define "cockroachdb.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 56 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create the name of the ServiceAccount to use.
*/}}
{{- define "cockroachdb.tls.serviceAccount.name" -}}
{{- if .Values.tls.serviceAccount.create -}}
    {{- default (include "cockroachdb.fullname" .) .Values.tls.serviceAccount.name -}}
{{- else -}}
    {{- default "default" .Values.tls.serviceAccount.name -}}
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

{{ compact (values $store) | sortAlpha | join "," }}
{{- end -}}
