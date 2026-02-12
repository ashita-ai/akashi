{{/*
Expand the name of the chart.
*/}}
{{- define "akashi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "akashi.fullname" -}}
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
{{- define "akashi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "akashi.labels" -}}
helm.sh/chart: {{ include "akashi.chart" . }}
{{ include "akashi.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "akashi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "akashi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "akashi.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "akashi.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Ollama URL — internal service or host.docker.internal fallback.
*/}}
{{- define "akashi.ollamaUrl" -}}
{{- if .Values.ollama.enabled }}
{{- printf "http://%s-ollama:11434" (include "akashi.fullname" .) }}
{{- else }}
{{- "http://localhost:11434" }}
{{- end }}
{{- end }}

{{/*
Database URL — through PgBouncer if enabled, direct otherwise.
*/}}
{{- define "akashi.databaseUrl" -}}
{{- if .Values.pgbouncer.enabled }}
{{- printf "postgres://%s:%s@%s-pgbouncer:5432/%s?sslmode=disable" .Values.database.user .Values.database.password (include "akashi.fullname" .) .Values.database.name }}
{{- else }}
{{- .Values.database.url }}
{{- end }}
{{- end }}
