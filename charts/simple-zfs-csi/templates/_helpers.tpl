{{/*
Expand the name of the chart.
*/}}
{{- define "simple-zfs-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Full release name.
*/}}
{{- define "simple-zfs-csi.fullname" -}}
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
{{- define "simple-zfs-csi.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "simple-zfs-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: simple-zfs-csi
{{- end -}}

{{/*
Resolve the image reference for a component ("nfs", "nvmeof", ...).
The values key is `component`; the derived image suffix is `suffix` (defaults to
`component`) so camelCase values keys can map to hyphenated image names.
Usage: {{ include "simple-zfs-csi.image" (dict "root" . "component" "nfs") }}
       {{ include "simple-zfs-csi.image" (dict "root" . "component" "csiController" "suffix" "controller") }}
*/}}
{{- define "simple-zfs-csi.image" -}}
{{- $root := .root -}}
{{- $component := .component -}}
{{- $suffix := .suffix | default $component -}}
{{- $comp := index $root.Values $component -}}
{{- if $comp.image.repository -}}
{{- $tag := $comp.image.tag | default $root.Values.image.tag | default $root.Chart.AppVersion -}}
{{- printf "%s:%s" $comp.image.repository $tag -}}
{{- else -}}
{{- $tag := $comp.image.tag | default $root.Values.image.tag | default $root.Chart.AppVersion -}}
{{- printf "%s/%s-%s:%s" $root.Values.image.registry $root.Values.image.repository $suffix $tag -}}
{{- end -}}
{{- end -}}

{{/*
ServiceAccount name for a component.
*/}}
{{- define "simple-zfs-csi.serviceAccountName" -}}
{{- printf "%s-%s" (include "simple-zfs-csi.fullname" .root) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Resolve the PriorityClass name for a component ("nfs", "nvmeof", "csiNode").
Falls back to the top-level `priorityClassName` when the component override is
unset. Usage: {{ include "simple-zfs-csi.priorityClassName" (dict "root" . "component" "nfs") }}
*/}}
{{- define "simple-zfs-csi.priorityClassName" -}}
{{- $root := .root -}}
{{- $comp := index $root.Values .component -}}
{{- $comp.priorityClassName | default $root.Values.priorityClassName -}}
{{- end -}}
