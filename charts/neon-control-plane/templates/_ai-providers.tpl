{{- define "neonControl.providerEnv" -}}
{{- if .root.Values.aiProviderManagement.enabled }}
- {name: NEON_AI_PROVIDER_MANAGEMENT_ENABLED, value: 'true'}
- {name: NEON_AI_PROVIDER_POLICY_FILE, value: /run/provider-public/policy.json}
- {name: NEON_AI_PROVIDER_PUBLIC_KEYRING_FILE, value: /run/provider-public/public-keyring.json}
{{- if .private }}
- {name: NEON_AI_PROVIDER_PRIVATE_KEYRING_FILE, value: /run/provider-private/private-keyring.json}
{{- end }}
{{- end }}
{{- end -}}
{{- define "neonControl.providerMounts" -}}
{{- if .root.Values.aiProviderManagement.enabled }}
- {name: provider-public, mountPath: /run/provider-public, readOnly: true}
{{- if .private }}
- {name: provider-private, mountPath: /run/provider-private, readOnly: true}
{{- end }}
{{- end }}
{{- end -}}
{{- define "neonControl.providerVolumes" -}}
{{- if .root.Values.aiProviderManagement.enabled }}
- name: provider-public
  secret:
    secretName: {{ required "aiProviderManagement.existingPublicSecret required" .root.Values.aiProviderManagement.existingPublicSecret }}
    defaultMode: 0440
    items: [{key: policy.json, path: policy.json}, {key: public-keyring.json, path: public-keyring.json}]
{{- if .private }}
- name: provider-private
  secret:
    secretName: {{ required "aiProviderManagement.existingPrivateSecret required" .root.Values.aiProviderManagement.existingPrivateSecret }}
    defaultMode: 0440
    items: [{key: private-keyring.json, path: private-keyring.json}]
{{- end }}
{{- end }}
{{- end -}}
