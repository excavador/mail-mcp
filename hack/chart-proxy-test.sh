#!/usr/bin/env bash
# The resource-proxy checks of `just chart-proxy`: two sidecar containers, one
# per resource, each pinned and bound to its own resource URL and upstream
# path, and the server behind them reachable on loopback only. Exercised with
# refusals as well: the checks that matter are the ones expecting failure.
#
# Run through the Justfile, which passes the shared fixtures:
#   issuer         --set auth.issuerUrl=...
#   proxy_values   --set proxy.image.tag=... --set proxy.image.digest=...
set -euo pipefail
issuer=${issuer:?run through just chart-proxy}
proxy_values=${proxy_values:?run through just chart-proxy}
base="--set accounts.existingSecret=x $issuer --set auth.readResourceUrl=https://mcp.example/mail --set auth.adminResourceUrl=https://mcp.example/mail-admin $proxy_values"
out=$(mktemp -d); trap 'rm -rf "$out"' EXIT
fail() { echo "FAIL: $*"; exit 1; }
refuses() { # name, then helm args: the render must fail
    local name=$1; shift
    if helm template t charts/mail-mcp "$@" >/dev/null 2>&1; then fail "rendered with $name"; fi
}
helm template t charts/mail-mcp $base --set pdfExtractor.enabled=true \
    --set httpRoute.enabled=true --set 'httpRoute.parentRefs[0].name=mcp' \
    --set 'httpRoute.hostnames[0]=mcp.example' > "$out/all.yaml"

# container(file, name): the text of one container of the Deployment.
container() { awk -v n="$2" '$0 ~ "^        - name: "n"$" {p=1; next} p && /^        - name: / {exit} p && /^      volumes:/ {exit} p' "$1"; }
container "$out/all.yaml" proxy-read  > "$out/read.yaml"
container "$out/all.yaml" proxy-admin > "$out/admin.yaml"
container "$out/all.yaml" mail-mcp    > "$out/server.yaml"
[ -s "$out/read.yaml" ] && [ -s "$out/admin.yaml" ] && [ -s "$out/server.yaml" ] || fail "a container is missing"

# Two proxies, one resource each, each forwarding to its own upstream path.
grep -A1 'name: RESOURCE_URL' "$out/read.yaml"  | grep -q '"https://mcp.example/mail"'       || fail "proxy-read RESOURCE_URL"
grep -A1 'name: RESOURCE_URL' "$out/admin.yaml" | grep -q '"https://mcp.example/mail-admin"' || fail "proxy-admin RESOURCE_URL"
grep -A1 'name: UPSTREAM' "$out/read.yaml"  | grep -q '"http://127.0.0.1:8081/mcp"'       || fail "proxy-read UPSTREAM is not /mcp"
grep -A1 'name: UPSTREAM' "$out/admin.yaml" | grep -q '"http://127.0.0.1:8081/mcp-admin"' || fail "proxy-admin UPSTREAM is not /mcp-admin"
grep -A1 'name: LISTEN' "$out/read.yaml"  | grep -q '":8080"' || fail "proxy-read does not listen on 8080"
grep -A1 'name: LISTEN' "$out/admin.yaml" | grep -q '":8082"' || fail "proxy-admin does not listen on 8082"
for c in read admin; do
    grep -A1 'name: ISSUER_URL' "$out/$c.yaml" | grep -q '"https://access.example"' || fail "proxy-$c ISSUER_URL"
    grep -q 'image: "ghcr.io/truvity/sluis/resource-proxy:1.84.0@sha256:adf1c78e983f9c5f9b6eed362cd02b6ce4a6c814ff561894c25bd48e6fb98a89"' "$out/$c.yaml" || fail "proxy-$c image is not pinned by tag and digest"
    grep -A1 'readinessProbe' "$out/$c.yaml" | grep -q '/readyz' || fail "proxy-$c readiness is not /readyz"
    grep -A1 'livenessProbe' "$out/$c.yaml" | grep -q '/healthz' || fail "proxy-$c liveness is not /healthz"
done

# The server holds no auth config, listens on loopback, publishes no MCP
# port, and its probes are on the ops port.
grep -A1 'name: ADDR' "$out/server.yaml" | grep -q '"127.0.0.1:8081"' || fail "server ADDR is not loopback"
! grep -q -E 'ISSUER_URL|RESOURCE_URL|SCOPE' "$out/server.yaml" || fail "the server still carries auth settings"
! grep -q 'containerPort: 8081' "$out/all.yaml" || fail "the server's MCP port is a container port"
[ "$(grep -c 'containerPort:' "$out/server.yaml")" = 1 ] && grep -q 'containerPort: 9090' "$out/server.yaml" || fail "the server publishes more than the ops port"
! grep -E 'path: /healthz, port: (http|admin)' "$out/server.yaml" || fail "a server probe is on a proxy port"

# Only the proxies' ports are in the Service; the ops and MCP ports are not.
awk '/^kind: Service$/,/^---$/' "$out/all.yaml" > "$out/svc.yaml"
grep -q 'targetPort: http$' "$out/svc.yaml" && grep -q 'targetPort: admin$' "$out/svc.yaml" || fail "Service does not target the two proxies"
[ "$(grep -c 'targetPort:' "$out/svc.yaml")" = 2 ] || fail "Service exposes more than the two proxy ports"
! grep -v '^ *#' "$out/svc.yaml" | grep -q -E '8081|9090|ops'  || fail "Service exposes the server's or the ops port"

# PSA restricted on every container; no service-account token except the
# server's projected one (the Lease).
awk '/^      containers:/,/^      volumes:/' "$out/all.yaml" > "$out/containers.yaml"
[ "$(grep -c 'allowPrivilegeEscalation: false' "$out/containers.yaml")" = 4 ] || fail "a container may escalate privileges"
[ "$(grep -c -E 'capabilities: \{drop: \["ALL"\]\}|^              - ALL$' "$out/containers.yaml")" = 4 ] || fail "a container keeps capabilities"
grep -q 'automountServiceAccountToken: false' "$out/all.yaml" || fail "service-account token is mounted"
! grep -q 'kube-api-access' "$out/read.yaml" "$out/admin.yaml" || fail "a proxy mounts the service-account token"
grep -q 'runAsUser: 65532' "$out/read.yaml" || fail "proxy runAsUser"

# The route: each resource's paths to its own proxy port, nothing rewritten
# but the suffix form of the metadata; timeouts only on the MCP rules.
awk '/^kind: HTTPRoute$/,/^---$/' "$out/all.yaml" > "$out/route.yaml"
for want in 'value: /mail-admin}' 'value: /mail}' 'value: /.well-known/oauth-protected-resource/mail}' 'value: /.well-known/oauth-protected-resource/mail-admin}' 'value: /mail/.well-known/oauth-protected-resource}' 'value: /mail-admin/.well-known/oauth-protected-resource}'; do
    grep -q -F -e "$want" "$out/route.yaml" || fail "route lacks $want"
done
[ "$(grep -c 'port: 8080' "$out/route.yaml")" = 3 ] && [ "$(grep -c 'port: 8082' "$out/route.yaml")" = 3 ] || fail "route does not split 3 rules per proxy port"
[ "$(grep -c 'request: 180s' "$out/route.yaml")" = 2 ] || fail "request timeout is not on exactly the two MCP rules"
[ "$(grep -c 'ReplaceFullPath' "$out/route.yaml")" = 2 ] || fail "suffix metadata rewrite"
! grep -q 'ReplacePrefixMatch' "$out/route.yaml" || fail "the route still rewrites the path prefix"
# The read path's rules must reach port 8080 only, the admin's 8082 only.
awk '/^    - matches:/{blk=""} {blk=blk $0 "\n"} /weight: 1/{ admin = (blk ~ /\/mail-admin/); if (admin && blk ~ /port: 8080/) bad=1; if (!admin && blk ~ /port: 8082/) bad=1 } END{exit bad}' "$out/route.yaml" || fail "a read path reaches the admin proxy or the reverse"

# Session affinity is carried over and can be turned off.
grep -q 'name: Mcp-Session-Id' "$out/all.yaml" && grep -q 'kind: BackendTrafficPolicy' "$out/all.yaml" || fail "no session affinity"
renders() { # pattern, then helm args: the render must contain the pattern
    local pat=$1; shift
    : > "$out/r.yaml"; helm template t charts/mail-mcp "$@" > "$out/r.yaml"
    grep -q -E -e "$pat" "$out/r.yaml"
}
! renders BackendTrafficPolicy $base --set httpRoute.enabled=true --set 'httpRoute.parentRefs[0].name=mcp' \
    --set httpRoute.sessionAffinity.enabled=false || fail "sessionAffinity.enabled=false still renders the policy"
# Off by default: no route, no policy.
! renders 'kind: (HTTPRoute|BackendTrafficPolicy)' $base || fail "a route rendered while httpRoute.enabled is false"

# NetworkPolicy: ingress from the gateway on the proxies' two ports only;
# egress DNS, HTTPS and IMAP; the ops and MCP ports are not admitted.
awk '/^kind: NetworkPolicy$/,/^  egress:/' "$out/all.yaml" > "$out/np.yaml"
grep -q 'envoy-gateway-system' "$out/np.yaml" && grep -q 'port: 8080' "$out/np.yaml" && grep -q 'port: 8082' "$out/np.yaml" || fail "ingress is not the gateway on 8080 and 8082"
! grep -q -E 'port: (8081|9090)' "$out/np.yaml" || fail "ingress admits the server or ops port"
awk '/^  egress:/,0' "$out/all.yaml" | awk '/^---$/{exit} {print}' > "$out/egress.yaml"
for want in 'port: 53' 'port: 443' 'port: 993'; do grep -q -e "$want" "$out/egress.yaml" || fail "egress lacks $want"; done
renders 'port: 143' $base --set-json 'networkPolicy.extraEgress=[{"ports":[{"port":143,"protocol":"TCP"}]}]' || fail "extraEgress ignored"
renders 'port: 8443' $base --set auth.issuerUrl=https://access.example:8443 || fail "the issuer's non-443 port is not allowed"
! renders 'kind: NetworkPolicy' $base --set networkPolicy.enabled=false || fail "networkPolicy.enabled=false still renders it"

# Refusals.
refuses "no proxy.image.tag" --set accounts.existingSecret=x $issuer --set auth.readResourceUrl=https://mcp.example/mail --set auth.adminResourceUrl=https://mcp.example/mail-admin --set proxy.image.digest=sha256:adf1c78e983f9c5f9b6eed362cd02b6ce4a6c814ff561894c25bd48e6fb98a89
refuses "no proxy.image.digest" --set accounts.existingSecret=x $issuer --set auth.readResourceUrl=https://mcp.example/mail --set auth.adminResourceUrl=https://mcp.example/mail-admin --set proxy.image.tag=1.84.0
refuses "a malformed proxy.image.digest" $base --set proxy.image.digest=sha256:abc
refuses "a trailing slash on readResourceUrl" $base --set auth.readResourceUrl=https://mcp.example/mail/
refuses "a trailing slash on adminResourceUrl" $base --set auth.adminResourceUrl=https://mcp.example/mail-admin/
refuses "http on readResourceUrl" $base --set auth.readResourceUrl=http://mcp.example/mail
refuses "no path on adminResourceUrl" $base --set auth.adminResourceUrl=https://mcp.example
refuses "a query on readResourceUrl" $base --set 'auth.readResourceUrl=https://mcp.example/mail?x=1'
refuses "http on the issuer" $base --set auth.issuerUrl=http://access.example
refuses "the admin path inside the read path" $base --set auth.adminResourceUrl=https://mcp.example/mail/admin
refuses "the read path inside the admin path" $base --set auth.readResourceUrl=https://mcp.example/mail-admin/x
refuses "the same path on two hosts" $base --set auth.adminResourceUrl=https://other.example/mail
refuses "equal Service ports" $base --set service.port=8082
refuses "an empty networkPolicy.ingressFrom" $base --set-json 'networkPolicy.ingressFrom=[]'
refuses "httpRoute.enabled without parentRefs" $base --set httpRoute.enabled=true
refuses "a host outside httpRoute.hostnames" $base --set httpRoute.enabled=true --set 'httpRoute.parentRefs[0].name=mcp' --set 'httpRoute.hostnames[0]=other.example'
refuses "an unknown proxy key" $base --set proxy.typo=1
refuses "the removed server auth flags in values" $base --set auth.typo=1
echo "chart-proxy ok"
