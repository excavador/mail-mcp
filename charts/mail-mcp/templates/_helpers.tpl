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

{{/* The fixed addresses inside the pod.

       proxy-read   :8080            the read resource  -> 127.0.0.1:8081/mcp
       proxy-admin  :8082            the admin resource -> 127.0.0.1:8081/mcp-admin
       mail-mcp     127.0.0.1:8081   loopback, no container port, no token check
       mail-mcp ops :9090            /healthz only, for the kubelet

     The two proxies are the pod's only MCP ports; the Service targets them
     alone. */}}
{{- define "mail-mcp.readProxyPort" -}}8080{{- end -}}
{{- define "mail-mcp.adminProxyPort" -}}8082{{- end -}}
{{- define "mail-mcp.mcpListen" -}}127.0.0.1:8081{{- end -}}
{{- define "mail-mcp.opsPort" -}}9090{{- end -}}

{{/* The pinned resource-proxy image. tag and digest are both required (see
     mail-mcp.validate): the sidecar checks every token, so a moved tag must
     not change what guards the mailboxes. */}}
{{- define "mail-mcp.proxyImage" -}}
{{- $i := .Values.proxy.image -}}
{{- printf "%s:%s@%s" $i.repository $i.tag $i.digest -}}
{{- end -}}

{{/* The path of a URL (no trailing slash: validate refuses one). */}}
{{- define "mail-mcp.urlPath" -}}
{{- (urlParse .).path -}}
{{- end -}}

{{/* The host (no port) of a URL. */}}
{{- define "mail-mcp.urlHost" -}}
{{- regexReplaceAll ":[0-9]+$" (urlParse .).host "" -}}
{{- end -}}

{{/* The port of a URL: from the scheme when it names none. */}}
{{- define "mail-mcp.urlPort" -}}
{{- $u := urlParse . -}}
{{- $p := regexFind ":[0-9]+$" $u.host | trimPrefix ":" -}}
{{- default (ternary "80" "443" (eq $u.scheme "http")) $p -}}
{{- end -}}

{{/* One resource-proxy container. The caller passes the root context, the
     container name, its listen port, the resource URL and the upstream path.
     Both proxies share the pod's network and hold no service-account token. */}}
{{- define "mail-mcp.proxyContainer" -}}
- name: {{ .name }}
  image: {{ include "mail-mcp.proxyImage" .root | quote }}
  imagePullPolicy: {{ .root.Values.proxy.image.pullPolicy }}
  securityContext:
    runAsNonRoot: true
    runAsUser: {{ .root.Values.proxy.runAsUser }}
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities: {drop: ["ALL"]}
    seccompProfile: {type: RuntimeDefault}
  env:
    - name: LISTEN
      value: {{ printf ":%s" .port | quote }}
    - name: UPSTREAM
      value: {{ printf "http://%s%s" (include "mail-mcp.mcpListen" .root) .upstream | quote }}
    - name: ISSUER_URL
      value: {{ .root.Values.auth.issuerUrl | quote }}
    - name: RESOURCE_URL
      value: {{ .url | quote }}
    - name: SCOPE
      value: {{ .root.Values.auth.scope | quote }}
  ports:
    - name: {{ .portName }}
      containerPort: {{ .port }}
      protocol: TCP
  # Let the gateway drop this pod before the proxy stops accepting.
  lifecycle:
    preStop:
      sleep:
        seconds: 5
  livenessProbe:
    httpGet: {path: /healthz, port: {{ .portName }}}
  # /readyz waits for the issuer's discovery document, so an issuer outage
  # keeps a NEW pod from becoming Ready (and stalls a rollout); the old pod
  # keeps serving until the issuer is back.
  readinessProbe:
    httpGet: {path: /readyz, port: {{ .portName }}}
  resources: {{- toYaml .root.Values.proxy.resources | nindent 4 }}
{{- end -}}

{{/* Refuse to render without the facts the sidecars and the server cannot
     start without. A missing value here otherwise becomes a CrashLoopBackOff
     whose cause is one layer away in the pod log. */}}
{{- define "mail-mcp.validate" -}}
{{- $urlRe := "^https://[^/?#[:space:]]+/[^?#[:space:]]*[^/?#[:space:]]$" -}}
{{- if not .Values.accounts.existingSecret -}}
{{- fail "accounts.existingSecret is required: the accounts file and password files are never literals in values" -}}
{{- end -}}
{{- if not .Values.auth.issuerUrl -}}
{{- fail "auth.issuerUrl is required: the resource-proxy sidecars validate every request against the issuer" -}}
{{- end -}}
{{- if not (regexMatch "^https://[^#[:space:]]+$" .Values.auth.issuerUrl) -}}
{{- fail (printf "auth.issuerUrl %q must be an https URL with no fragment, exactly as it appears in a token's `iss`" .Values.auth.issuerUrl) -}}
{{- end -}}
{{- if not .Values.auth.readResourceUrl -}}
{{- fail "auth.readResourceUrl is required: it is the read endpoint's external URL, the RFC 8707 audience the issuer mints tokens for" -}}
{{- end -}}
{{- if not .Values.auth.adminResourceUrl -}}
{{- fail "auth.adminResourceUrl is required: it is the admin endpoint's external URL" -}}
{{- end -}}
{{- range $k := list "readResourceUrl" "adminResourceUrl" -}}
{{- $v := index $.Values.auth $k -}}
{{- if not (regexMatch $urlRe $v) -}}
{{- fail (printf "auth.%s %q must be an https URL with a path, and no trailing slash, query or fragment: the proxy requires every token's `aud` to equal it byte for byte, and the route is derived from its path" $k $v) -}}
{{- end -}}
{{- end -}}
{{- $rp := include "mail-mcp.urlPath" .Values.auth.readResourceUrl -}}
{{- $ap := include "mail-mcp.urlPath" .Values.auth.adminResourceUrl -}}
{{- if eq .Values.auth.readResourceUrl .Values.auth.adminResourceUrl -}}
{{- fail "auth.readResourceUrl and auth.adminResourceUrl must be different: one audience for both would hand a read-only token the write tools" -}}
{{- end -}}
{{- if or (eq $rp $ap) (hasPrefix (printf "%s/" $rp) (printf "%s/" $ap)) (hasPrefix (printf "%s/" $ap) (printf "%s/" $rp)) -}}
{{- fail (printf "the paths of auth.readResourceUrl (%s) and auth.adminResourceUrl (%s) must be distinct and neither inside the other: each proxy answers its own prefix, and the gateway sends a path to one of them" $rp $ap) -}}
{{- end -}}
{{- if eq (int .Values.service.port) (int .Values.service.adminPort) -}}
{{- fail "service.port and service.adminPort must differ: they are the Service ports of the read and the admin proxy" -}}
{{- end -}}
{{- if not .Values.proxy.image.tag -}}
{{- fail "proxy.image.tag is empty. The resource-proxy sidecar is built by another repository's release, so this chart carries no default version; pin the release you have verified." -}}
{{- end -}}
{{- if not .Values.proxy.image.digest -}}
{{- fail "proxy.image.digest is empty. The sidecars check every token and are pinned by tag AND digest, so a moved tag cannot change what guards the mailboxes." -}}
{{- end -}}
{{- if .Values.pdfExtractor.enabled -}}
{{- $mem := toString .Values.pdfExtractor.resources.limits.memory -}}
{{- if and (regexMatch "^[0-9]+Mi$" $mem) (lt (int (trimSuffix "Mi" $mem)) 464) -}}
{{- fail "pdfExtractor.resources.limits.memory must be at least 464Mi (the helper's 400 MB address-space cap plus 64Mi): below it the kernel OOM-kills pdftotext on large files" -}}
{{- end -}}
{{- end -}}
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.ingressFrom) -}}
{{- fail "networkPolicy.enabled is true but networkPolicy.ingressFrom is empty, so the policy would admit NOBODY to the proxies' ports, the gateway included. Name the gateway's peers, or set networkPolicy.enabled to false." -}}
{{- end -}}
{{- if .Values.httpRoute.enabled -}}
{{- if not .Values.httpRoute.parentRefs -}}
{{- fail "httpRoute.enabled is true but httpRoute.parentRefs is empty, so the route would attach to no Gateway and answer nothing. Name the Gateway or ListenerSet it attaches to." -}}
{{- end -}}
{{- range $k := list "readResourceUrl" "adminResourceUrl" -}}
{{- $host := include "mail-mcp.urlHost" (index $.Values.auth $k) -}}
{{- if and $.Values.httpRoute.hostnames (not (has $host $.Values.httpRoute.hostnames)) -}}
{{- fail (printf "the host %q of auth.%s is not in httpRoute.hostnames (%s); the route would not match the URL clients are told to use." $host $k (join ", " $.Values.httpRoute.hostnames)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
