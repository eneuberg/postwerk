{{/* Chart name. */}}
{{- define "postwerk.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Full name for resources. */}}
{{- define "postwerk.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "postwerk.selectorLabels" -}}
app.kubernetes.io/name: {{ include "postwerk.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "postwerk.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "postwerk.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "postwerk.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "postwerk.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "postwerk.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (toString .Values.image.tag) }}
{{- end }}
{{- end }}

{{/* Checks that fail the render with a clear message. */}}
{{- define "postwerk.validate" -}}
{{- if and (not .Values.tls.enabled) (not .Values.tls.allowInsecureAuth) }}
{{- fail "tls.enabled=false needs tls.allowInsecureAuth=true (cleartext AUTH; only on a network that encrypts traffic between nodes)" }}
{{- end }}
{{- if and .Values.tls.enabled .Values.tls.allowInsecureAuth }}
{{- fail "tls.enabled and tls.allowInsecureAuth together are ambiguous; choose one" }}
{{- end }}
{{- if and .Values.tls.certManager.enabled (not .Values.tls.enabled) }}
{{- fail "tls.certManager.enabled needs tls.enabled=true" }}
{{- end }}
{{- if .Values.ui.enabled }}
{{- $port := int .Values.ui.port }}
{{- if or (lt $port 1) (gt $port 65535) (eq $port 2525) (eq $port 8080) }}
{{- fail "ui.port must be a free port between 1 and 65535, not 2525 (SMTP) or 8080 (health)" }}
{{- end }}
{{- end }}
{{- if not .Values.targets }}
{{- fail "targets: at least one target is required" }}
{{- end }}
{{- range .Values.targets }}
{{- if not (regexMatch "^[a-z0-9_]{1,32}$" (toString .name)) }}
{{- fail (printf "targets: name %q must match [a-z0-9_]{1,32}" (toString .name)) }}
{{- end }}
{{- if not .url }}
{{- fail (printf "targets: %s needs a url" .name) }}
{{- end }}
{{- end }}
{{- end }}

{{/* Comma-separated target names for POSTWERK_TARGETS. */}}
{{- define "postwerk.targetNames" -}}
{{- $names := list }}
{{- range .Values.targets }}{{ $names = append $names .name }}{{ end }}
{{- join "," $names }}
{{- end }}
