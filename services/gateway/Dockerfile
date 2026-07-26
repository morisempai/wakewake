# Gateway service image.
#
# BUILD CONTEXT IS THE REPOSITORY ROOT, not this directory:
#
#     docker build -f services/gateway/Dockerfile -t gateway .
#
# This module's go.mod carries `replace` directives pointing at ../../shared, so a build context
# scoped to services/gateway/ cannot resolve them. The COPY lines below bring in exactly the two
# shared modules this binary needs (platform, and contracts which platform requires) and nothing
# else, so a change to another service does not invalidate this image's layers.

# Build stage. The version matches go.work's `go 1.26.0` — service-template requires the two to move
# together, because a toolchain ahead of the workspace directive builds locally and fails in CI.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependency manifests first, so `go mod download` is cached and only re-runs when a dependency
# actually changes rather than on every source edit.
COPY shared/contracts/go.mod shared/contracts/go.sum ./shared/contracts/
COPY shared/platform/go.mod shared/platform/go.sum ./shared/platform/
COPY services/gateway/go.mod services/gateway/go.sum ./services/gateway/

# GOWORK=off, and go.work is deliberately NOT copied: the workspace lists every service, so building
# inside it would require all of their sources and would rebuild this image whenever an unrelated
# service changed. The replace directives in go.mod already describe everything this binary needs.
ENV GOWORK=off
RUN cd services/gateway && go mod download

COPY shared/contracts ./shared/contracts
COPY shared/platform ./shared/platform
COPY services/gateway ./services/gateway

# CGO_ENABLED=0 for a static binary that runs on distroless/static, which has no libc. -trimpath
# keeps build-machine paths out of the binary; -s -w drop the symbol table and DWARF.
RUN cd services/gateway && \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/gateway ./cmd/gateway

# Runtime stage. distroless/static has no shell, no package manager, and no interpreters, so a
# remote-code-execution bug has almost nothing to execute.
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=build /out/gateway /gateway

# nonroot (uid 65532), provided by the base image. The service binds 8080 rather than a privileged
# port precisely so it never needs more.
USER nonroot:nonroot
EXPOSE 8080

# The binary probes itself: there is no curl or wget in this image. Liveness only — /readyz would
# restart the container whenever Keycloak blinked.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/gateway", "healthcheck"]

ENTRYPOINT ["/gateway"]
