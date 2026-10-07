{{- define "neonControl.runtimeEnv" -}}
{{- if .Values.managedAuth.enabled }}
{{- if or (not .Values.managedAuth.labHTTP) (not .Values.managedAuth.publicOrigin) }}
{{- fail "Managed Auth v1 requires explicit managedAuth.labHTTP and publicOrigin; trusted TLS has not passed" }}
{{- end }}
{{- if or (not .Values.managedAuth.pgCASecret) (not .Values.managedAuth.pgServerName) }}
{{- fail "Managed Auth requires an explicit SQL CA Secret and certificate DNS identity" }}
{{- end }}
- {name: NEON_AUTH_ENABLED, value: 'true'}
- {name: NEON_AUTH_LAB_HTTP, value: 'true'}
- {name: NEON_AUTH_RUNTIME_IMAGE, value: {{ .Values.managedAuth.runtimeImage | quote }}}
- {name: NEON_AUTH_PUBLIC_ORIGIN, value: {{ .Values.managedAuth.publicOrigin | quote }}}
- {name: NEON_AUTH_PG_CA_SECRET, value: {{ .Values.managedAuth.pgCASecret | quote }}}
- {name: NEON_AUTH_PG_CA_KEY, value: {{ .Values.managedAuth.pgCAKey | quote }}}
- {name: NEON_AUTH_PG_SERVER_NAME, value: {{ .Values.managedAuth.pgServerName | quote }}}
{{- end }}
{{- if and .Values.api.pitrEnabled (not .Values.api.creationEnabled) }}
{{- fail "Historical branch restore requires api.creationEnabled" }}
{{- end }}
{{- if .Values.dataAPI.enabled }}
{{- if not .Values.dataAPI.labHTTP }}
{{- fail "Data API native Driver v1 requires explicit dataAPI.labHTTP; trusted TLS has not passed" }}
{{- end }}
- {name: NEON_DATA_API_ENABLED, value: 'true'}
- {name: NEON_DATA_API_LAB_HTTP, value: 'true'}
- {name: NEON_DATA_API_GATEWAY_IMAGE, value: {{ .Values.dataAPI.gatewayImage | quote }}}
- {name: NEON_DATA_API_POSTGREST_IMAGE, value: {{ .Values.dataAPI.postgrestImage | quote }}}
{{- end }}
- {name: NEON_KUBE_NAMESPACE, value: {{ .Release.Namespace | quote }}}
- {name: NEON_V2_CREATE_ENABLED, value: {{ .Values.api.creationEnabled | quote }}}
- {name: NEON_V2_PITR_ENABLED, value: {{ .Values.api.pitrEnabled | quote }}}
- {name: NEON_V2_SCALE_ZERO_ENABLED, value: {{ .Values.api.scaleToZeroEnabled | quote }}}
- {name: NEON_COOKIE_SECURE, value: {{ .Values.api.cookieSecure | quote }}}
- {name: NEON_PROXY_HOST, value: {{ .Values.api.proxyHost | quote }}}
- {name: NEON_PROXY_PORT, value: {{ .Values.api.proxyPort | quote }}}
- {name: NEON_PUBLIC_PROXY_HOST, value: {{ .Values.api.publicProxyHost | quote }}}
- {name: NEON_PUBLIC_PROXY_PORT, value: {{ .Values.api.publicProxyPort | quote }}}
- {name: NEON_PG_TLS_MODE, value: {{ .Values.api.pgTLSMode | quote }}}
- {name: NEON_VM_COMPUTE_IMAGE, value: {{ .Values.api.computeImage | quote }}}
- {name: NEON_COMPUTE_GATEWAY_URL, value: {{ .Values.api.computeGatewayURL | quote }}}
- {name: NEON_COMPUTE_CONTROL_HOST, value: {{ .Values.api.computeControlHost | quote }}}
- {name: NEON_V2_IDEMPOTENCY_KEY_FILE, value: /run/neon-secrets/idempotency-key}
- name: NEON_V2_DATABASE_URL
  valueFrom: {secretKeyRef: {name: {{ .Values.api.existingSecret }}, key: database-url}}
{{- if .Values.api.pgCASecret }}
- {name: NEON_PG_CA_FILE, value: /run/pg-ca/ca.crt}
{{- end }}
{{- end -}}
