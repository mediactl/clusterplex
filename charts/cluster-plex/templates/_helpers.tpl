{{- define "cluster-plex.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cluster-plex.labels" -}}
app.kubernetes.io/name: {{ include "cluster-plex.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "cluster-plex.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{/*
The proxy Service must not be reachable through nodes that are not running a
proxy pod. With the default Cluster policy, kube-proxy forwards to pods on
other nodes and masquerades the source, so traffic hairpins across the very
links this design exists to scale, and the client address Plex sees is wrong.
This is a property of the design, not a preference, so the chart refuses to
render if it is overridden.
*/}}
{{- define "cluster-plex.externalTrafficPolicy" -}}
{{- $given := default "Local" .Values.proxy.service.externalTrafficPolicy -}}
{{- if ne $given "Local" -}}
{{- fail "proxy.service.externalTrafficPolicy must be Local: with Cluster, media traffic hairpins between nodes and client addresses are masqueraded. See docs/media-proxy-pattern.md" -}}
{{- end -}}
Local
{{- end -}}
