{{- define "neonControl.name" -}}
{{- default (printf "%s-control" .Release.Name) .Values.fullnameOverride | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- define "neonControl.labels" -}}
app.kubernetes.io/name: neon-control-plane
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}
{{- define "neonControl.platformPolicy" -}}
{{- dict "version" 1 "operator_ids" .Values.platformManagement.operatorIds "allowed_organizations" .Values.platformManagement.allowedOrganizations "plan_management" true | toJson -}}
{{- end -}}
{{- define "neonControl.platformPolicyName" -}}
{{- printf "%s-platform-%s" (include "neonControl.name" . | trunc 36 | trimSuffix "-") (include "neonControl.platformPolicy" . | sha256sum | trunc 16) -}}
{{- end -}}
