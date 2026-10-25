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

{{/* Two payload slots must fit on the configured private disk volume. */}}
{{- define "neonControl.storageCapacityEnv" -}}
{{- if .Values.objectStorage.enabled -}}
{{- $capacity := .Values.objectStorage.capacity -}}
{{- if or (lt (int64 $capacity.branchBytes) (int64 $capacity.objectBytes)) (lt (mul (int64 $capacity.bufferMiB) 1048576) (mul (int64 $capacity.objectBytes) 2)) -}}
{{- fail "objectStorage.capacity branch and two-slot buffer must cover objectBytes" -}}
{{- end }}
- {name: NEON_STORAGE_OBJECT_BYTES, value: {{ $capacity.objectBytes | int64 | quote }}}
- {name: NEON_STORAGE_BRANCH_BYTES, value: {{ $capacity.branchBytes | int64 | quote }}}
- {name: NEON_STORAGE_BUFFER_BYTES, value: {{ mul (int64 $capacity.bufferMiB) 1048576 | quote }}}
- {name: NEON_STORAGE_TRANSFER_SECONDS, value: {{ $capacity.transferSeconds | int64 | quote }}}
{{- end -}}
{{- end -}}
