{{/*
  Blocks upgrades where selfSignedOperatorCerts is toggled in either direction
  while a conflicting cockroach-operator-certs Secret already exists.

  false → true: the chart previously provisioned the Secret (Helm-managed). The operator
    cannot self-generate this Secret while a Helm-managed copy exists, causing a CrashLoopBackOff.

  true → false: the operator previously created the Secret (no Helm ownership markers).
    Helm cannot take ownership of a Secret it did not create, causing an upgrade
    failure with an ownership conflict error.

  Detection checks any of the standard Helm ownership markers that Helm stamps on every
  resource it manages: the meta.helm.sh/release-name and meta.helm.sh/release-namespace
  annotations, or the app.kubernetes.io/managed-by=Helm label. Checking all three ensures
  existing deployments from any prior chart version are correctly identified as Helm-managed.
*/}}
{{- define "operator.selfSignerConflictValidation" -}}
{{- if .Release.IsUpgrade -}}
  {{- $secret := lookup "v1" "Secret" .Release.Namespace "cockroach-operator-certs" -}}
  {{- if $secret -}}
    {{- $annotations := $secret.metadata.annotations | default dict -}}
    {{- $labels := $secret.metadata.labels | default dict -}}
    {{- $helmProvisioned := or
          (index $annotations "meta.helm.sh/release-name")
          (index $annotations "meta.helm.sh/release-namespace")
          (eq (index $labels "app.kubernetes.io/managed-by") "Helm") -}}
    {{- if and .Values.selfSignedOperatorCerts $helmProvisioned -}}
      {{- fail (printf "Upgrade blocked: selfSignedOperatorCerts is now true, but the 'cockroach-operator-certs' Secret in namespace '%s' was previously provisioned by Helm. The operator cannot self-generate this Secret while a Helm-managed copy exists. Remove it first:\n\n  kubectl delete secret cockroach-operator-certs -n %s\n\nThen re-run the upgrade." .Release.Namespace .Release.Namespace) -}}
    {{- end -}}
    {{- if and (not .Values.selfSignedOperatorCerts) (not $helmProvisioned) -}}
      {{- fail (printf "Upgrade blocked: selfSignedOperatorCerts is now false, but the 'cockroach-operator-certs' Secret in namespace '%s' was previously created by the operator. Helm cannot take ownership of this Secret. Remove it first:\n\n  kubectl delete secret cockroach-operator-certs -n %s\n\nThen re-run the upgrade. A new certificate will be generated and managed by Helm." .Release.Namespace .Release.Namespace) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
  Generates a CA and a signed cert for SQL connections (root
  cert).
  By convention, the first line is expected to be the ca.crt
  entry. Lines 2-3 are the client.root entries. Finally, lines
  4-5 are the client.node entries.
*/}}
{{- define "operator.certs" -}}
{{- $days := default .Values.certificate.validForDays 3650 | int -}}
{{- $ca := genCA "cockroach-operator-certs" 3650 -}}
{{- $cert := genSignedCert "cert" nil (list (printf "cockroach-webhook-service.%s.svc" .Release.Namespace) (printf "cockroach-operator.%s.svc" .Release.Namespace)) $days $ca -}}
ca.crt: {{ $ca.Cert | b64enc }}
tls.crt: {{ $cert.Cert | b64enc }}
tls.key: {{ $cert.Key | b64enc }}
{{- end }}
