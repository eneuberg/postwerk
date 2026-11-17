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
{{- printf "%s:%s" .Values.image.repository (toString (.Values.image.tag | default .Chart.AppVersion)) }}
{{- end }}
{{- end }}

{{/* Checks that fail the render with a clear message. */}}
{{- define "postwerk.validate" -}}
{{- if not (has (toString .Values.senderKeys) (list "file" "db" "both")) }}
{{- fail "senderKeys must be file, db or both" }}
{{- end }}
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
{{- if .Values.api.enabled }}
{{- $port := int .Values.api.port }}
{{- if or (lt $port 1) (gt $port 65535) (eq $port 2525) (eq $port 8080) (and .Values.ui.enabled (eq $port (int .Values.ui.port))) }}
{{- fail "api.port must be a free port between 1 and 65535, not 2525 (SMTP), 8080 (health) or ui.port" }}
{{- end }}
{{- if and .Values.api.tls.enabled (not .Values.api.tls.secretName) (not .Values.tls.enabled) }}
{{- fail "api.tls.enabled needs api.tls.secretName, or tls.enabled=true to share the SMTP certificate" }}
{{- end }}
{{- end }}
{{- if .Values.sentry.enabled }}
{{- if not (has (toString .Values.sentry.minLevel) (list "error" "warn")) }}
{{- fail "sentry.minLevel must be error or warn" }}
{{- end }}
{{- if not .Values.secrets.sentryDsn.name }}
{{- fail "sentry.enabled needs secrets.sentryDsn (an existing Secret with the DSN)" }}
{{- end }}
{{- end }}
{{- if .Values.networkPolicy.enabled }}
{{- /*
An egress rule with ports but no peers ("to" empty) allows those ports to
every address. Never render one by accident: "any address" must be written
out as cidrs [0.0.0.0/0, ::/0] (D59).
*/}}
{{- $eg := .Values.networkPolicy.egress }}
{{- if and $eg.dns.enabled (not $eg.dns.to) }}
{{- fail "networkPolicy.egress.dns.to is empty: the DNS rule would allow port 53 to every address. Name the DNS pods, or set networkPolicy.egress.dns.enabled=false" }}
{{- end }}
{{- if and $eg.targetPorts (not $eg.targetCIDRs) }}
{{- fail "networkPolicy.egress.targetCIDRs is empty: the rule for targetPorts would allow every address. List your provider's ranges, write [0.0.0.0/0, \"::/0\"] for any address, or set targetPorts: [] and reach the targets through networkPolicy.egress.extra" }}
{{- end }}
{{- if and .Values.sentry.enabled $eg.sentry.enabled (not $eg.sentry.cidrs) }}
{{- fail "networkPolicy.egress.sentry.cidrs is empty: the Sentry rule would allow port 443 to every address. List your Sentry host's ranges, write [0.0.0.0/0, \"::/0\"] for any address, or set networkPolicy.egress.sentry.enabled=false and reach it through networkPolicy.egress.extra" }}
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

{{/* Host names for POSTWERK_API_HOSTS: the Service names and api.hosts. */}}
{{- define "postwerk.apiHosts" -}}
{{- $svc := include "postwerk.fullname" . }}
{{- $ns := .Release.Namespace }}
{{- $hosts := list $svc (printf "%s.%s" $svc $ns) (printf "%s.%s.svc" $svc $ns) (printf "%s.%s.svc.%s" $svc $ns .Values.clusterDomain) }}
{{- range .Values.api.hosts }}{{ $hosts = append $hosts (lower .) }}{{ end }}
{{- join "," $hosts }}
{{- end }}

{{/* Comma-separated target names for POSTWERK_TARGETS. */}}
{{- define "postwerk.targetNames" -}}
{{- $names := list }}
{{- range .Values.targets }}{{ $names = append $names .name }}{{ end }}
{{- join "," $names }}
{{- end }}
