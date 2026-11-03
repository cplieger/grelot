# check=error=true
FROM golang:1.27-alpine@sha256:7d5cbf6833f7331dafd25a2e8b9673477f559759ff8ed4ca8efabe6795ad08db AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /grelot .
COPY LICENSE NOTICE ./
COPY scripts/collect-licenses.sh scripts/
RUN --mount=type=cache,target=/go/pkg/mod \
    sh scripts/collect-licenses.sh --name grelot .

FROM scratch

COPY --from=builder --chmod=755 /grelot /grelot
COPY --from=builder /out/usr/share/licenses /usr/share/licenses

USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD ["/grelot", "health"]
ENTRYPOINT ["/grelot"]
CMD ["serve"]
