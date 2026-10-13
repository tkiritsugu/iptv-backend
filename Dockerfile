# syntax=docker/dockerfile:1

# Build stage. The final image contains no toolchain and no sources.
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies are copied first so the module download layer is reused while
# only Go files change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off: the SQLite driver is pure Go, so the result is a static binary that
# runs in a minimal image with no libc assumptions.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/iptv ./cmd/iptv

# Run stage.
FROM debian:stable-slim

# A non-root user whose home is the data directory, so the SQLite file it creates
# is writable without privileges.
RUN useradd --system --uid 65532 --user-group --create-home --home-dir /data --shell /usr/sbin/nologin iptv

COPY --from=build /out/iptv /usr/local/bin/iptv

USER iptv
WORKDIR /data

ENV IPTV_ADDR=:8080 \
    IPTV_DB=/data/iptv.db

EXPOSE 8080

# The binary probes itself: no shell, curl or python needed in the image.
HEALTHCHECK --interval=10s --timeout=4s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/iptv", "-healthcheck", "-healthcheck-path", "/readyz"]

ENTRYPOINT ["/usr/local/bin/iptv"]
