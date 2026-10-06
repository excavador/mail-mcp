# The PDF text sidecar of mail-mcp: the pdftext helper plus poppler's
# pdftotext, nothing else. Build from the repository root:
#   docker buildx build -f cmd/pdftext/Dockerfile .
#
# The Go stage always runs on the build host and cross-compiles, so the arm64
# image does not need an emulated Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/pdftext ./cmd/pdftext
COPY internal/pdftext ./internal/pdftext
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/pdftext ./cmd/pdftext

FROM alpine:3.22
RUN apk add --no-cache poppler-utils \
 && rm -rf /var/cache/apk/* /usr/share/man /usr/share/doc
COPY --from=build /out/pdftext /usr/local/bin/pdftext
ARG VERSION=dev
LABEL org.opencontainers.image.title="mail-mcp-pdftext" \
      org.opencontainers.image.description="Sandboxed PDF text extraction sidecar for mail-mcp" \
      org.opencontainers.image.source="https://github.com/excavador/mail-mcp" \
      org.opencontainers.image.licenses="MIT AND GPL-2.0-or-later" \
      org.opencontainers.image.version="$VERSION"
# Same uid as the mail-mcp pod (chart podSecurityContext): the shared socket
# and the blob store are owner-only.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pdftext"]
