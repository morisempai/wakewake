# Catalog service image.
#
# BUILD CONTEXT IS THE REPOSITORY ROOT, not this directory:
#
#     docker build -f services/catalog/Dockerfile -t catalog .
#
# This module's go.mod carries `replace` directives pointing at ../../shared, so a build context
# scoped to services/catalog/ cannot resolve them. The COPY lines below bring in exactly the three
# shared modules that are required and nothing else, so a change to another service does not
# invalidate this image's layers.

# Build stage. The version matches go.work's `go 1.26.0` — service-template requires the two to
# move together, because a toolchain ahead of the workspace directive builds locally and fails in
# CI with a message about neither.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependency manifests first, so `go mod download` is cached and only re-runs when a dependency
# actually changes rather than on every source edit.
COPY shared/contracts/go.mod shared/contracts/go.sum ./shared/contracts/
COPY shared/platform/go.mod shared/platform/go.sum ./shared/platform/
COPY shared/testkit/go.mod shared/testkit/go.sum ./shared/testkit/
COPY services/catalog/go.mod services/catalog/go.sum ./services/catalog/

# GOWORK=off, and go.work is deliberately NOT copied: the workspace lists every service, so
# building inside it would require all of their sources to be present and would rebuild this image
# whenever an unrelated service changed. The replace directives in go.mod already describe
# everything this binary needs.
#
# shared/testkit's manifests are copied even though the binary does not use it, because go.mod
# requires it and module resolution fails on a replace target that is not there. Only its
# manifests — not its source, and not the testcontainers tree behind it — enter this image.
ENV GOWORK=off
RUN cd services/catalog && go mod download

COPY shared/contracts ./shared/contracts
COPY shared/platform ./shared/platform
COPY services/catalog ./services/catalog

# CGO_ENABLED=0 for a static binary that runs on distroless/static, which has no libc. -trimpath
# keeps build-machine paths out of the binary; -s -w drop the symbol table and DWARF, which is a
# large share of the image for a binary nobody debugs in production.
RUN cd services/catalog && \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/catalog ./cmd/catalog

# Runtime stage. distroless/static has no shell, no package manager, and no interpreters, so a
# remote-code-execution bug has almost nothing to execute.
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=build /out/catalog /catalog

# nonroot (uid 65532), provided by the base image. The service binds 8080 rather than a privileged
# port precisely so it never needs more.
USER nonroot:nonroot
EXPOSE 8080

# The binary probes itself: there is no curl or wget in this image, and adding a shell to answer
# "is the process alive" would cost more than the check is worth. Liveness only — /readyz would
# restart the container whenever a dependency blinked.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/catalog", "healthcheck"]

ENTRYPOINT ["/catalog"]
