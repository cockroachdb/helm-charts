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
{{- $cert := genSignedCert "cert" nil (list "cockroach-webhook-service.default.svc" "cockroach-operator.default.svc") $days $ca -}}
ca.crt: {{ $ca.Cert | b64enc }}
tls.crt: {{ $cert.Cert | b64enc }}
tls.key: {{ $cert.Key | b64enc }}
{{- end }}
