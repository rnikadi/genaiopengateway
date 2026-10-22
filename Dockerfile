# Builds natively on the build machine and cross-compiles for each target
# platform, so multi-arch images need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/gateway", "-config", "/etc/genaiopengateway/config.yaml"]
