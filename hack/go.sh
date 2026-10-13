#!/bin/sh
# Run the Go toolchain inside Docker — the host Go installation is never used.
#
#   ./hack/go.sh build ./...
#   ./hack/go.sh test -race ./...
#   ./hack/go.sh vet ./...
#   ./hack/go.sh run ./cmd/iptv        (publishes :8080 to the host)
#
# Module/build caches and GOPATH live under .devcache/ in the project, so nothing
# is written outside the repository.
set -eu

IMAGE=${GO_IMAGE:-golang:1.26-bookworm}
SRC=$(cd "$(dirname "$0")/.." && pwd)

# `run` and `test` that need to listen on a port get the port published.
PORTS=""
if [ "${1:-}" = "run" ]; then
	PORTS="-p 8080:8080"
fi

exec docker run --rm \
	--user "$(id -u):$(id -g)" \
	-e HOME=/src/.devcache/home \
	-e GOMODCACHE=/src/.devcache/mod \
	-e GOCACHE=/src/.devcache/build \
	-e GOPATH=/src/.devcache/gopath \
	-e GOFLAGS=-mod=mod \
	-e CGO_ENABLED=0 \
	-v "$SRC":/src \
	-w /src $PORTS \
	"$IMAGE" go "$@"
