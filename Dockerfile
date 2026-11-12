# Builds natively on the build machine and cross-compiles for each target
# platform, so multi-arch images need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/gogctl ./cmd/gogctl
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
# gogctl runs the Kubernetes operator (docs/operator.md) and admin tasks.
COPY --from=build /out/gogctl /gogctl
# Without a mounted config file the gateway starts with zero config and
# keeps its SQLite database and generated secrets here (docs/quickstart.md).
COPY --from=build --chown=nonroot:nonroot /out/data /var/lib/genaiopengateway
ENV GOG_DATA_DIR=/var/lib/genaiopengateway
VOLUME /var/lib/genaiopengateway
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/gateway", "-config", "/etc/genaiopengateway/config.yaml"]
