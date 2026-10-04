{{- define "neonControl.runtimeEnv" -}}
- {name: NEON_KUBE_NAMESPACE, value: {{ .Release.Namespace | quote }}}
- {name: NEON_V2_CREATE_ENABLED, value: {{ .Values.api.creationEnabled | quote }}}
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
