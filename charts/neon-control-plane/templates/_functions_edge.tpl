{{- define "neonControl.functionEdgeBrokerEnv" -}}
{{- if (dig "edge" "enabled" false .Values.functions) }}
- {name: NEON_FUNCTIONS_EDGE_ENABLED, value: 'true'}
- {name: NEON_FUNCTIONS_EDGE_BROKER_BIND, value: ':8795'}
- {name: NEON_FUNCTIONS_EDGE_BROKER_CERT_FILE, value: /run/function-edge-broker/tls.crt}
- {name: NEON_FUNCTIONS_EDGE_BROKER_KEY_FILE, value: /run/function-edge-broker/tls.key}
- {name: NEON_FUNCTIONS_EDGE_CLIENT_CA_FILE, value: /run/function-edge-broker/client-ca.crt}
{{- end }}
{{- end -}}

{{- define "neonControl.functionEdgeBrokerMounts" -}}
{{- if (dig "edge" "enabled" false .Values.functions) }}
- {name: function-edge-broker, mountPath: /run/function-edge-broker, readOnly: true}
{{- end }}
{{- end -}}

{{- define "neonControl.functionEdgeBrokerVolumes" -}}
{{- if (dig "edge" "enabled" false .Values.functions) }}
- name: function-edge-broker
  secret:
    secretName: {{ required "functions.edge.existingBrokerSecret is required" .Values.functions.edge.existingBrokerSecret }}
    defaultMode: 288
    items: [{key: tls.crt, path: tls.crt}, {key: tls.key, path: tls.key}, {key: client-ca.crt, path: client-ca.crt}]
{{- end }}
{{- end -}}
