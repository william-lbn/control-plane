{{/*
DSGA runtime catalog is an API-only control-plane capability. The Worker
continues an already-frozen operation and must not receive the current catalog
Secret; this prevents unnecessary release-catalog disclosure to the Worker.
*/}}
{{- define "neonControl.dsgaApiEnv" -}}
{{- if and .Values.dsga.createEnabled (not .Values.api.creationEnabled) }}
{{- fail "dsga.createEnabled requires api.creationEnabled" }}
{{- end }}
{{- if and .Values.dsga.createEnabled (not .Values.dsga.runtimeCatalogSecret) }}
{{- fail "dsga.createEnabled requires dsga.runtimeCatalogSecret" }}
{{- end }}
- {name: NEON_DSGA_CREATE_ENABLED, value: {{ .Values.dsga.createEnabled | quote }}}
{{- if .Values.dsga.runtimeCatalogSecret }}
- name: NEON_DSGA_RUNTIME_CATALOG
  valueFrom:
    secretKeyRef: {name: {{ .Values.dsga.runtimeCatalogSecret | quote }}, key: catalog.json}
{{- end }}
{{- end -}}
