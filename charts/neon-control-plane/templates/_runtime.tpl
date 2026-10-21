{{- define "neonControl.runtimeEnv" -}}
{{- if and .Values.functions.managerCertificateRenewal.enabled (not .Values.functions.enabled) }}
{{- fail "Manager certificate renewal requires functions.enabled" }}
{{- end }}
{{- if and .Values.functions.branchCopies.enabled (not .Values.functions.enabled) }}
{{- fail "Function branch copies require functions.enabled" }}
{{- end }}
{{- if and .Values.functions.outputLogs.enabled (not .Values.functions.enabled) }}
{{- fail "Function output collection requires functions.enabled" }}
{{- end }}
{{- if and .Values.functions.schedules.enabled (not .Values.functions.enabled) }}
{{- fail "Functions schedules require functions.enabled" }}
{{- end }}
{{- if and .Values.functions.objectEvents.enabled (or (not .Values.functions.enabled) (not .Values.functions.publicHTTP.enabled) (not .Values.objectStorage.enabled)) }}
{{- fail "Functions object events require Functions, public HTTP, and Object Storage" }}
{{- end }}
{{- if and .Values.objectStorage.s3.enabled (not .Values.objectStorage.enabled) }}
{{- fail "S3 requires objectStorage.enabled" }}
{{- end }}
{{- if .Values.functions.enabled }}
{{- if and .Values.functions.schedules.enabled (not .Values.functions.publicHTTP.enabled) }}
{{- fail "Functions schedules require functions.publicHTTP.enabled" }}
{{- end }}
- {name: NEON_FUNCTION_SCHEDULES_ENABLED, value: {{ .Values.functions.schedules.enabled | quote }}}
- {name: NEON_FUNCTION_OBJECT_EVENTS_ENABLED, value: {{ .Values.functions.objectEvents.enabled | quote }}}
- {name: NEON_FUNCTION_OUTPUT_ENABLED, value: {{ .Values.functions.outputLogs.enabled | quote }}}
- {name: NEON_FUNCTION_BRANCH_COPY_ENABLED, value: {{ .Values.functions.branchCopies.enabled | quote }}}
- {name: NEON_FUNCTION_MANAGER_RENEWAL_ENABLED, value: {{ .Values.functions.managerCertificateRenewal.enabled | quote }}}
- {name: NEON_FUNCTION_MANAGER_RENEW_BEFORE_SECONDS, value: {{ .Values.functions.managerCertificateRenewal.renewBeforeSeconds | quote }}}
- {name: NEON_FUNCTIONS_PREVIEW_ENABLED, value: 'true'}
- {name: NEON_FUNCTIONS_CONFIG_FILE, value: /run/functions-common/config.json}
{{- if .Values.functions.publicHTTP.enabled }}
{{- if not (regexMatch "^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?$" .Values.functions.publicHTTP.origin) }}
{{- fail "Functions origin must be a path-free HTTP(S) IPv4/DNS authority" }}
{{- end }}
{{- $functionHost := lower (regexReplaceAll "^https?://([^/:]+)(:[0-9]+)?$" .Values.functions.publicHTTP.origin "${1}") }}
{{- $consoleOrigin := required "Functions requires managedAuth.publicOrigin as the Console origin reference, including when Auth is disabled" .Values.managedAuth.publicOrigin }}
{{- $consoleHost := lower (regexReplaceAll "^https?://([^/:]+)(:[0-9]+)?$" $consoleOrigin "${1}") }}
{{- if and $consoleHost (eq $functionHost $consoleHost) }}
{{- fail "Functions requires a separate hostname/IP; ports alone do not isolate browser cookies" }}
{{- end }}
{{- if eq .Values.functions.publicHTTP.origin .Values.managedAuth.publicOrigin }}
{{- fail "Functions HTTP must use a separate origin from Console/Auth" }}
{{- end }}
{{- if and (hasPrefix "http://" .Values.functions.publicHTTP.origin) (not .Values.functions.publicHTTP.labHTTP) }}
{{- fail "Functions cleartext requires explicit functions.publicHTTP.labHTTP" }}
{{- end }}
- {name: NEON_FUNCTIONS_PUBLIC_ORIGIN, value: {{ required "Functions publicHTTP.origin is required" .Values.functions.publicHTTP.origin | quote }}}
- {name: NEON_FUNCTIONS_PUBLIC_BIND, value: ':8792'}
- {name: NEON_FUNCTIONS_PUBLIC_LAB_HTTP, value: {{ .Values.functions.publicHTTP.labHTTP | quote }}}
{{- end }}
{{- end }}
{{- if .Values.objectStorage.enabled }}
- {name: NEON_OBJECT_STORAGE_CONFIG_FILE, value: /run/product-storage/config.json}
{{- if .Values.objectStorage.s3.enabled }}
{{- if not (regexMatch "^https?://[a-zA-Z0-9.-]+(:[0-9]{1,5})?$" .Values.objectStorage.s3.origin) }}
{{- fail "S3 origin must be a path-free HTTP(S) IPv4/DNS authority" }}
{{- end }}
{{- if not .Values.backendCredentials.enabled }}
{{- fail "S3 requires backendCredentials.enabled" }}
{{- end }}
{{- if and (hasPrefix "http://" .Values.objectStorage.s3.origin) (not .Values.objectStorage.s3.labHTTP) }}
{{- fail "S3 cleartext requires explicit objectStorage.s3.labHTTP" }}
{{- end }}
- {name: NEON_S3_PUBLIC_ORIGIN, value: {{ required "S3 origin required" .Values.objectStorage.s3.origin | quote }}}
- {name: NEON_S3_REGION, value: {{ .Values.objectStorage.s3.region | quote }}}
- {name: NEON_S3_LAB_HTTP, value: {{ .Values.objectStorage.s3.labHTTP | quote }}}
- {name: NEON_S3_BIND, value: ':8793'}
{{- end }}
{{- end }}
{{- if .Values.managedAuth.enabled }}
{{- if .Values.managedAuth.mail.enabled }}
- {name: NEON_AUTH_MAIL_ENABLED, value: 'true'}
- {name: NEON_AUTH_MAIL_SECRET, value: {{ required "Auth mail requires managedAuth.mail.existingSecret" .Values.managedAuth.mail.existingSecret | quote }}}
{{- end }}
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
