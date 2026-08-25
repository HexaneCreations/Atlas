# Atlas production image.
#
# Two properties matter here and drive every choice below:
#
#   1. The image contains a binary and nothing else. Atlas reads a host's most
#      sensitive surfaces; an attacker who reaches the container should find no
#      shell, no package manager, and no interpreter to pivot with.
#   2. The build is reproducible. -trimpath removes local paths, and the
#      version stamp comes from build arguments rather than from whatever the
#      builder happened to have checked out.

# ----------------------------------------------------------------- build ---
FROM golang:1.25-alpine AS build

# Dependencies are resolved in their own layer, so a source-only change does
# not re-download the module graph.
WORKDIR /src
COPY go.mod go.sum ./
# proxy.golang.org occasionally drops mid-download with an HTTP/2
# INTERNAL_ERROR; GODEBUG disables HTTP/2 to the proxy and the retry loop
# absorbs the rest.
ENV GODEBUG=http2client=0
RUN for i in 1 2 3; do go mod download && break || sleep 5; done

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILDTIME=unknown
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# CGO_ENABLED=0 produces a fully static binary, which is what allows the
# runtime stage to be scratch rather than a distribution base image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w \
        -X 'github.com/hexane/atlas/internal/platform/build.Version=${VERSION}' \
        -X 'github.com/hexane/atlas/internal/platform/build.Commit=${COMMIT}' \
        -X 'github.com/hexane/atlas/internal/platform/build.BuildTime=${BUILDTIME}'" \
      -o /out/atlas-server ./cmd/atlas-server

# ------------------------------------------------------------- certificates ---
# Even a scratch image needs CA certificates to verify a TLS connection to
# Postgres, and a passwd entry so the binary can run as a named non-root user.
FROM alpine:3.21 AS certs
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 -g atlas atlas

# --------------------------------------------------------------- runtime ---
FROM scratch

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=certs /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=certs /etc/passwd /etc/passwd
COPY --from=build /out/atlas-server /usr/local/bin/atlas-server

# Unprivileged by construction. Atlas is read-only and, in this deployment
# shape, needs no host access at all — the collectors that do come with the
# agent, whose deployment is documented separately.
USER atlas

# The container listens on all interfaces because the container *is* the
# network boundary; the loopback default protects a developer laptop, not
# this. Publish the port deliberately.
ENV ATLAS_SERVER_HOST=0.0.0.0 \
    ATLAS_SERVER_PORT=8080 \
    ATLAS_ENVIRONMENT=production \
    ATLAS_LOGGING_FORMAT=json

EXPOSE 8080

# No shell exists in this image, so the health check is the binary probing
# itself is not possible either — orchestrators should use the HTTP probes at
# /healthz and /readyz, which is what they do natively.
ENTRYPOINT ["/usr/local/bin/atlas-server"]
CMD ["serve"]
