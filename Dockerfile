# syntax=docker/dockerfile:1

# Build stage. The toolchain never reaches the final image, which is the
# difference between a ~1GB container and a ~10MB one.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies are copied and downloaded first so this layer stays cached
# across source-only changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Version identifiers are stamped in at build time. This is genuinely useful in
# operation: it lets anyone establish exactly which commit a running container
# was built from, without guessing from image tags.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# TARGETOS/TARGETARCH are supplied by buildx for each platform in the matrix.
ARG TARGETOS
ARG TARGETARCH

# CGO is disabled so the result is a genuinely static binary with no libc
# dependency, which is what allows the distroless static base below.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.buildDate=${BUILD_DATE}" \
      -o /out/gmc-exporter \
      ./cmd/gmc-exporter

# Runtime stage.
#
# distroless/static carries no shell, no package manager and no libc, so the
# attack surface is the binary itself. :nonroot runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gmc-exporter /usr/local/bin/gmc-exporter

# Running as a non-root user is the default here. Reading /dev/ttyUSB0 does not
# require root: it requires membership of the group owning the device node,
# which on most hosts is dialout. Pass the device through and add the group:
#
#   docker run --device=/dev/ttyUSB0 --group-add=$(stat -c '%g' /dev/ttyUSB0) ...
#
# That is preferable to --privileged, which grants every capability on the host
# in order to solve a file-permission problem.
USER nonroot:nonroot

EXPOSE 9101

# The health endpoints are plain HTTP, but distroless has no shell or curl to
# call them with, so HEALTHCHECK is left to the orchestrator or an external
# monitor pointed at /readyz.

ENTRYPOINT ["/usr/local/bin/gmc-exporter"]

LABEL org.opencontainers.image.title="gmc-exporter" \
      org.opencontainers.image.description="Export GQ Electronics GMC-series Geiger counter readings to Prometheus, InfluxDB, MQTT/Home Assistant, OTLP, radmon.org, GMCMAP and Safecast" \
      org.opencontainers.image.source="https://github.com/RealDougEubanks/gmc-exporter" \
      org.opencontainers.image.licenses="MIT"
