# check=error=true
FROM golang:1.27-alpine@sha256:7d5cbf6833f7331dafd25a2e8b9673477f559759ff8ed4ca8efabe6795ad08db AS builder
ENV GOTOOLCHAIN=auto

ARG BUILD_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# The @cplieger libraries publish TypeScript source only: each is extracted
# into the front end's node_modules for cmd/bundle to resolve. Each ARG pins
# the exact version internal/webui/static-src/package.json pins, and the fetch
# refuses a mismatch by name, since Renovate moves the two pins separately.
# renovate: datasource=npm depName=@cplieger/ui
ARG CPLIEGER_UI_VERSION=1.3.0-dev.28
# renovate: datasource=npm depName=@cplieger/reactive
ARG CPLIEGER_REACTIVE_VERSION=2.4.0
# renovate: datasource=npm depName=@cplieger/sse
ARG CPLIEGER_SSE_VERSION=2.2.0
# renovate: datasource=npm depName=@cplieger/access
ARG CPLIEGER_ACCESS_VERSION=1.0.0-dev.12
COPY internal/webui/static-src/package.json internal/webui/static-src/
RUN set -e; \
    fetch() { \
      pinned=$(sed -n "s|.*\"@cplieger/$1\": \"\([^\"]*\)\".*|\1|p" internal/webui/static-src/package.json); \
      if [ "$pinned" != "$2" ]; then \
        echo "ERROR $1-pin-mismatch: internal/webui/static-src/package.json pins @cplieger/$1 ${pinned:-nothing} but the Dockerfile ARG pins $2" >&2; \
        exit 1; \
      fi; \
      dir="internal/webui/static-src/node_modules/@cplieger/$1"; \
      mkdir -p "$dir"; \
      wget -qO /tmp/pkg.tgz "https://registry.npmjs.org/@cplieger/$1/-/$1-$2.tgz"; \
      tar -xzf /tmp/pkg.tgz -C "$dir" --strip-components=1; \
      rm /tmp/pkg.tgz; \
    }; \
    fetch ui "${CPLIEGER_UI_VERSION}"; \
    fetch reactive "${CPLIEGER_REACTIVE_VERSION}"; \
    fetch sse "${CPLIEGER_SSE_VERSION}"; \
    fetch access "${CPLIEGER_ACCESS_VERSION}"

COPY *.go ./
COPY cmd/ cmd/
COPY internal/ internal/
COPY providers/ providers/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go run ./cmd/bundle && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${BUILD_VERSION}" -o /grelot .
COPY LICENSE NOTICE ./
COPY scripts/collect-licenses.sh scripts/
RUN --mount=type=cache,target=/go/pkg/mod \
    sh scripts/collect-licenses.sh --name grelot .

# The store's directory for the scratch stage, owned by the runtime user.
RUN mkdir -p /outfs/data

FROM scratch

# CA bundle for a provider door that pushes over https.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder --chown=65532:65532 --chmod=700 /outfs/data /data
COPY --from=builder --chmod=755 /grelot /grelot
COPY --from=builder /out/usr/share/licenses /usr/share/licenses

USER 65532:65532
VOLUME /data
EXPOSE 9737 9738
# `grelot health` GETs /healthz at OPS_LISTEN_ADDR and gives up at 5 s with its
# reason, so the timeout sits above that budget and Docker never cuts it first.
HEALTHCHECK --interval=30s --timeout=10s --retries=3 --start-period=15s \
    CMD ["/grelot", "health"]
ENTRYPOINT ["/grelot"]
CMD ["serve"]
