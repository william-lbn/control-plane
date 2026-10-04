{{- define "neonControl.name" -}}
{{- default (printf "%s-control" .Release.Name) .Values.fullnameOverride | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- define "neonControl.labels" -}}
app.kubernetes.io/name: neon-control-plane
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}
