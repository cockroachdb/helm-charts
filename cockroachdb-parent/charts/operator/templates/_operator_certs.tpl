{{/*
  Blocks upgrades where selfSignedOperatorCerts is switched from false to true
  while a chart-provisioned cockroach-operator-certs Secret still exists. Without
  this guard, the operator starts, finds the pre-existing Secret, and enters
  a CrashLoopBackOff. Detection relies on the cockroach.crdb.io/provisioned-by
  annotation that this chart stamps on the Secret it creates, distinguishing it
  from Secrets created by the operator itself.
*/}}
{{- define "operator.selfSignerConflictValidation" -}}
{{- if and .Values.selfSignedOperatorCerts .Release.IsUpgrade -}}
  {{- $secret := lookup "v1" "Secret" .Release.Namespace "cockroach-operator-certs" -}}
  {{- if $secret -}}
    {{- $annotations := $secret.metadata.annotations | default dict -}}
    {{- if index $annotations "cockroach.crdb.io/provisioned-by" -}}
      {{- fail (printf "Upgrade blocked: selfSignedOperatorCerts is now true, but the 'cockroach-operator-certs' Secret in namespace '%s' was previously provisioned by Helm. The operator cannot self-generate this Secret while a Helm-managed copy exists. Remove it first:\n\n  kubectl delete secret cockroach-operator-certs -n %s\n\nThen re-run the upgrade." .Release.Namespace .Release.Namespace) -}}
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
