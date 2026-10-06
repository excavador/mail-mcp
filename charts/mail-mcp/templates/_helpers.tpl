{{- define "mail-mcp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The standard Helm idiom, and not merely cosmetic: naively joining
     release and chart name yields mail-mcp-mail-mcp for the obvious
     release name, and a route pointing at "mail-mcp" then fails with
     BackendNotFound -- which stays invisible behind a gateway that rejects
     unauthenticated requests before it ever routes them. */}}
{{- define "mail-mcp.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "mail-mcp.labels" -}}
app.kubernetes.io/name: {{ include "mail-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "mail-mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mail-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Refuse to render without the facts the server cannot start without.
     A missing value here otherwise becomes a CrashLoopBackOff whose cause is
     one layer away in the pod log. */}}
{{- define "mail-mcp.validate" -}}
{{- if not .Values.accounts.existingSecret -}}
{{- fail "accounts.existingSecret is required: the accounts file and password files are never literals in values" -}}
{{- end -}}
{{- if not .Values.auth.issuerUrl -}}
{{- fail "auth.issuerUrl is required: the server validates every request against access-roster itself" -}}
{{- end -}}
{{- if not .Values.auth.readResourceUrl -}}
{{- fail "auth.readResourceUrl is required: it is this server's read endpoint external URL, the RFC 8707 audience access-roster mints tokens for" -}}
{{- end -}}
{{- if not .Values.auth.adminResourceUrl -}}
{{- fail "auth.adminResourceUrl is required: it is this server's admin endpoint external URL" -}}
{{- end -}}
{{- if eq .Values.auth.readResourceUrl .Values.auth.adminResourceUrl -}}
{{- fail "auth.readResourceUrl and auth.adminResourceUrl must be different" -}}
{{- end -}}
{{- if .Values.pdfExtractor.enabled -}}
{{- $mem := toString .Values.pdfExtractor.resources.limits.memory -}}
{{- if and (regexMatch "^[0-9]+Mi$" $mem) (lt (int (trimSuffix "Mi" $mem)) 464) -}}
{{- fail "pdfExtractor.resources.limits.memory must be at least 464Mi (the helper's 400 MB address-space cap plus 64Mi): below it the kernel OOM-kills pdftotext on large files" -}}
{{- end -}}
{{- end -}}
{{- end -}}
