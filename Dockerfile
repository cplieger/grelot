# check=error=true
FROM golang:1.27-alpine@sha256:7d5cbf6833f7331dafd25a2e8b9673477f559759ff8ed4ca8efabe6795ad08db AS builder
ENV GOTOOLCHAIN=auto

ARG BUILD_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
COPY internal/ internal/
COPY providers/ providers/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
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
EXPOSE 9737
# `grelot health` GETs /healthz at LISTEN_ADDR and gives up at 5 s with its
# reason, so the timeout sits above that budget and Docker never cuts it first.
HEALTHCHECK --interval=30s --timeout=10s --retries=3 --start-period=15s \
    CMD ["/grelot", "health"]
ENTRYPOINT ["/grelot"]
CMD ["serve"]
