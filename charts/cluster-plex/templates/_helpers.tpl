{{- define "cluster-plex.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The prefix of every resource name. The release name by default, as before;
fullnameOverride lets a parent chart install this one beside its own
resources (clustarr's chart sets "plex").
*/}}
{{- define "cluster-plex.fullname" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
A value rendered as a template, so a parent chart can name its own
resources ("{{ .Release.Name }}-plex-db-rw"). Call with (list $value $).
*/}}
{{- define "cluster-plex.tpl" -}}
{{- tpl (toString (index . 0)) (index . 1) -}}
{{- end -}}

{{/* The library's claim: storage.media.existingClaim (a template), else <fullname>-media. */}}
{{- define "cluster-plex.mediaClaim" -}}
{{- with .Values.storage.media.existingClaim -}}
{{- include "cluster-plex.tpl" (list . $) -}}
{{- else -}}
{{- include "cluster-plex.fullname" . -}}-media
{{- end -}}
{{- end -}}

{{/* clustarr's namespace: plex.clustarr.namespace (a template), else this release's. */}}
{{- define "cluster-plex.clustarrNamespace" -}}
{{- with .Values.plex.clustarr.namespace -}}
{{- include "cluster-plex.tpl" (list . $) -}}
{{- else -}}
{{- .Release.Namespace -}}
{{- end -}}
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
externalTrafficPolicy for a LoadBalancer Service: Local, and nothing else.
Call with (list $given $key $reason). Under Cluster, kube-proxy forwards to
pods on other nodes and replaces the source address with a node's. For the
client Service that breaks the session pinning, which hashes the client's
address; for the media proxy's it also hairpins media across the links that
tier exists to scale. A property of the design, not a preference, so the
chart refuses to render it overridden.
*/}}
{{- define "cluster-plex.externalTrafficPolicy" -}}
{{- $given := default "Local" (index . 0) -}}
{{- if ne $given "Local" -}}
{{- fail (printf "%s must be Local: %s" (index . 1) (index . 2)) -}}
{{- end -}}
Local
{{- end -}}

{{/*
Values chart 0.3.0 moved. Each would otherwise be dropped without a word, and
the first one is the failure docs/configuration.md warns about: Plex then
advertises another address, which works until a client signs in.
*/}}
{{- define "cluster-plex.validate" -}}
{{- if .Values.proxy.externalURL -}}
{{- fail "proxy.externalURL moved to plex.externalURL in chart 0.3.0: it is the address Plex advertises, whatever is in front" -}}
{{- end -}}
{{- if and (not .Values.proxy.enabled) (or .Values.proxy.service.loadBalancerIP .Values.proxy.service.annotations) -}}
{{- fail "proxy.service.* is the media proxy's own Service since chart 0.3.0, rendered only with proxy.enabled; the Service clients connect to is configured under service.*" -}}
{{- end -}}
{{- end -}}
