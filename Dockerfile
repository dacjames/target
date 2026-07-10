# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.25-alpine AS build
WORKDIR /src
# Manifests first for layer caching (go.sum needed to fetch golang.org/x/net).
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/target .

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
