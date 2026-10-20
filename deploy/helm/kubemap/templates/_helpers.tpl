{{/* Labels used by every object; same as deploy/kubemap.yaml. */}}
{{- define "kubemap.labels" -}}
app.kubernetes.io/managed-by: kubemap
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/part-of: kubemap
{{- end -}}

{{- define "kubemap.image" -}}
{{ required "image.repository is required" .Values.image.repository }}:{{ required "image.tag is required" .Values.image.tag }}
{{- end -}}

{{/* Fail early on a wrong release namespace or an unsafe host/issuer. */}}
{{- define "kubemap.validate" -}}
{{- if ne .Release.Namespace "kubemap-system" -}}
{{- fail "kubemap must be installed in the namespace kubemap-system: use -n kubemap-system --create-namespace" -}}
{{- end -}}
{{- if .Values.ingress.enabled -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$" (toString .Values.ingress.host)) -}}
{{- fail "ingress.host must be a DNS name (lower-case letters, digits, dots, dashes)" -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$" (toString .Values.ingress.issuer)) -}}
{{- fail "ingress.issuer must be a ClusterIssuer name" -}}
{{- end -}}
{{- end -}}
{{- if and .Values.trustedProxies (not (regexMatch "^[0-9a-fA-F:./, ]+$" (toString .Values.trustedProxies))) -}}
{{- fail "trustedProxies must be comma-separated CIDRs or IPs" -}}
{{- end -}}
{{- end -}}
