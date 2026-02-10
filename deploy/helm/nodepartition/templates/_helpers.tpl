{{/*
Expand the name of the chart.
*/}}
{{- define "nodepartition.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "nodepartition.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "nodepartition.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "nodepartition.labels" -}}
helm.sh/chart: {{ include "nodepartition.chart" . }}
{{ include "nodepartition.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "nodepartition.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nodepartition.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Controller labels.
*/}}
{{- define "nodepartition.controller.labels" -}}
{{ include "nodepartition.labels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Controller selector labels.
*/}}
{{- define "nodepartition.controller.selectorLabels" -}}
{{ include "nodepartition.selectorLabels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Controller service account name.
*/}}
{{- define "nodepartition.controller.serviceAccountName" -}}
{{- if .Values.controller.serviceAccount.create }}
{{- default (printf "%s-controller" (include "nodepartition.fullname" .)) .Values.controller.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.controller.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Controller image.
*/}}
{{- define "nodepartition.controller.image" -}}
{{- printf "%s:%s" .Values.controller.image.repository (default .Chart.AppVersion .Values.controller.image.tag) }}
{{- end }}
