{{/*
Use the conventional shared root only when independent stores need common trust.
Persistent single-server installations retain their existing CA unless overridden.
*/}}
{{- define "cilium.spire.upstreamSecret" -}}
{{- $server := .Values.authentication.mutual.spire.install.server -}}
{{- if $server.ca.upstreamSecret -}}
{{- $server.ca.upstreamSecret -}}
{{- else if and .Values.encryption.ztunnel.spire.useCRD (or (gt (int64 $server.replicas) 1) (not $server.dataStorage.enabled)) -}}
spire-upstream-ca
{{- end -}}
{{- end -}}
