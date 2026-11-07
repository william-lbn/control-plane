{{- define "neonControl.storageGatewayService" -}}
{{- $s3 := .Values.objectStorage.s3 -}}
{{- $gateway := default "" $s3.gatewayService -}}
{{- if $gateway -}}
{{- if not (and .Values.objectStorage.enabled $s3.enabled) -}}
{{- fail "Storage gateway takeover requires objectStorage.enabled and objectStorage.s3.enabled" -}}
{{- end -}}
{{- if $s3.apiListenerEnabled -}}
{{- fail "Storage gateway takeover requires objectStorage.s3.apiListenerEnabled=false" -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?([.][a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$" $gateway) -}}
{{- fail "objectStorage.s3.gatewayService must be an exact Kubernetes Service DNS name without scheme, port or path" -}}
{{- end -}}
{{- else if and .Values.objectStorage.enabled $s3.enabled (not $s3.apiListenerEnabled) -}}
{{- fail "Disabling the API S3 listener requires objectStorage.s3.gatewayService for S3 and signed REST takeover" -}}
{{- end -}}
{{- $gateway -}}
{{- end -}}
