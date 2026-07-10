# syntax=docker/dockerfile:1

# ---- build stage ----
# Build on the native builder arch, cross-compile to the target arch — fast
# multi-arch builds without QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
# Manifests first for layer caching (go.sum needed to fetch golang.org/x/net).
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -X main.version=$VERSION" -o /out/target .

# ---- run stage ----
FROM alpine:3.20
# ca-certificates for HTTPS callbacks; iputils for a real ping (ICMP callbacks).
RUN apk add --no-cache ca-certificates iputils
WORKDIR /app
COPY --from=build /out/target /usr/local/bin/target
COPY targets.json /app/targets.json
ENV TARGET_CONFIG=/app/targets.json
# Ports match the shipped targets.json (high ports = unprivileged).
EXPOSE 8081 8082 8443 9090 9091 8053/udp
ENTRYPOINT ["target"]
