{{- define "neonControl.functionsMounts" -}}
{{- if .Values.functions.enabled }}
- {name: functions-common, mountPath: /run/functions-common, readOnly: true}
- {name: functions-proxy, mountPath: /run/functions-proxy, readOnly: true}
{{- end }}
{{- end -}}

{{- define "neonControl.functionsVolumes" -}}
{{- if .Values.functions.enabled }}
- name: functions-common
  secret:
    secretName: {{ required "functions.existingConfigSecret is required" .Values.functions.existingConfigSecret }}
    defaultMode: 0440
    items:
      - {key: config.json, path: config.json}
      - {key: backing-mac, path: backing-mac}
      - {key: guest-ca.crt, path: guest-ca.crt}
- name: functions-proxy
  secret:
    secretName: {{ required "functions.proxyCASecret is required" .Values.functions.proxyCASecret }}
    defaultMode: 0440
    # Project only a public certificate, never the Proxy TLS private key.
    items: [{key: {{ .Values.functions.proxyCAKey | quote }}, path: ca.crt}]
{{- end }}
{{- end -}}
