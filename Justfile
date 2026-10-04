# Everything goes through here; CI runs the same recipes.

# Build and vet.
#
# Builds the COMMAND explicitly, not just ./... -- a repo whose main package is
# missing still passes `go build ./...`, because the library packages compile
# on their own.
check:
    go vet ./...
    go test ./...
    go build ./...
    go build -o /dev/null ./cmd/mail-mcp
    just chart

# Lint and render the chart, and prove its guards still refuse bad input.
#
# A chart exercised only with correct values has not been tested: the schema
# and the required-value guards exist to REJECT things, so the checks that
# matter are the ones expecting failure.
auth_values := "--set auth.issuerUrl=https://access.example --set auth.readResourceUrl=https://mcp.example/read --set auth.adminResourceUrl=https://mcp.example/admin"

chart:
    helm lint charts/mail-mcp \
        --set accounts.existingSecret=x {{auth_values}}
    helm template t charts/mail-mcp \
        --set accounts.existingSecret=x {{auth_values}} > /dev/null
    @# missing required values
    @! helm template t charts/mail-mcp >/dev/null 2>&1 \
        || (echo "FAIL: rendered without accounts.existingSecret"; exit 1)
    @# auth.issuerUrl is required
    @! helm template t charts/mail-mcp --set accounts.existingSecret=x \
        --set auth.readResourceUrl=https://mcp.example/read --set auth.adminResourceUrl=https://mcp.example/admin \
        >/dev/null 2>&1 \
        || (echo "FAIL: rendered without auth.issuerUrl"; exit 1)
    @# auth.readResourceUrl is required
    @! helm template t charts/mail-mcp --set accounts.existingSecret=x \
        --set auth.issuerUrl=https://access.example --set auth.adminResourceUrl=https://mcp.example/admin \
        >/dev/null 2>&1 \
        || (echo "FAIL: rendered without auth.readResourceUrl"; exit 1)
    @# auth.adminResourceUrl is required
    @! helm template t charts/mail-mcp --set accounts.existingSecret=x \
        --set auth.issuerUrl=https://access.example --set auth.readResourceUrl=https://mcp.example/read \
        >/dev/null 2>&1 \
        || (echo "FAIL: rendered without auth.adminResourceUrl"; exit 1)
    @# read and admin resource URLs must not be equal
    @! helm template t charts/mail-mcp --set accounts.existingSecret=x \
        --set auth.issuerUrl=https://access.example --set auth.readResourceUrl=https://mcp.example/same \
        --set auth.adminResourceUrl=https://mcp.example/same \
        >/dev/null 2>&1 \
        || (echo "FAIL: accepted equal read and admin resource URLs"; exit 1)
    @# unknown keys are typos, not options
    @! helm template t charts/mail-mcp --set accounts.existingSecret=x \
        {{auth_values}} --set unknownKey=true >/dev/null 2>&1 \
        || (echo "FAIL: accepted an unknown values key"; exit 1)
    @# reloader reads its annotation on the Deployment, not the pod template.
    @# An annotation that lands in the wrong place looks right and never fires.
    @helm template t charts/mail-mcp --set accounts.existingSecret=x {{auth_values}} \
        --set-string 'deploymentAnnotations.reloader\.stakater\.com/auto=true' \
      | awk '/^kind: Deployment/,/^spec:/' | grep -q 'reloader.stakater.com/auto' \
      || (echo "FAIL: deploymentAnnotations did not reach the Deployment"; exit 1)
    @# auth.scope defaults to openid and reaches SCOPE, so an estate that
    @# never sets it still advertises a scope access-roster accepts.
    @helm template t charts/mail-mcp --set accounts.existingSecret=x {{auth_values}} \
      | grep -A1 'name: SCOPE' | grep -q '"openid"' \
      || (echo "FAIL: auth.scope did not default to openid on SCOPE"; exit 1)
    @# and an estate needing a different scope can still set it.
    @helm template t charts/mail-mcp --set accounts.existingSecret=x \
        {{auth_values}} --set auth.scope=openid+mail \
      | grep -A1 'name: SCOPE' | grep -q '"openid+mail"' \
      || (echo "FAIL: auth.scope did not override SCOPE"; exit 1)
    @echo "chart ok: renders, refuses missing values (including auth and accounts), equal resource URLs, and unknown keys, annotates the Deployment, and wires auth.scope"

# What the release will build, without publishing.
#
# KO_DOCKER_REPO is required even when publishing is skipped -- ko needs a
# repository to name the image it builds, and fails before building without
# one. CI passes the real registry; locally any name will do.
snapshot:
    KO_DOCKER_REPO=ko.local goreleaser release --snapshot --clean --skip=publish
