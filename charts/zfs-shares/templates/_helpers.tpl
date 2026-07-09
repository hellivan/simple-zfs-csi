{{/*
Expand the name of the chart.
*/}}
{{- define "zfs-shares.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Full release name.
*/}}
{{- define "zfs-shares.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "zfs-shares.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "zfs-shares.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: zfs-shares
{{- end -}}

{{/*
Resolve the image reference for a component ("nfs" or "nvmeof").
Usage: {{ include "zfs-shares.image" (dict "root" . "component" "nfs") }}
*/}}
{{- define "zfs-shares.image" -}}
{{- $root := .root -}}
{{- $component := .component -}}
{{- $comp := index $root.Values $component -}}
{{- if $comp.image.repository -}}
{{- $tag := $comp.image.tag | default $root.Values.image.tag | default $root.Chart.AppVersion -}}
{{- printf "%s:%s" $comp.image.repository $tag -}}
{{- else -}}
{{- $tag := $comp.image.tag | default $root.Values.image.tag | default $root.Chart.AppVersion -}}
{{- printf "%s/%s-%s:%s" $root.Values.image.registry $root.Values.image.repository $component $tag -}}
{{- end -}}
{{- end -}}

{{/*
ServiceAccount name for a component.
*/}}
{{- define "zfs-shares.serviceAccountName" -}}
{{- printf "%s-%s" (include "zfs-shares.fullname" .root) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}
