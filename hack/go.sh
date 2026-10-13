#!/bin/sh
# Run the Go toolchain inside Docker — the host Go installation is never used.
#
#   ./hack/go.sh build ./...
#   ./hack/go.sh test ./...
#   ./hack/go.sh vet ./...
#   ./hack/go.sh run ./cmd/iptv        (publishes :8080 to the host)
#
# The race detector needs cgo, so opt in explicitly:
#
#   CGO_ENABLED=1 ./hack/go.sh test -race ./...
#
# Module/build caches and GOPATH live under .devcache/ in the project, so nothing
# is written outside the repository.
set -eu

IMAGE=${GO_IMAGE:-golang:1.26-bookworm}
SRC=$(cd "$(dirname "$0")/.." && pwd)

# "go run" of the server gets the port published so the host can reach it.
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
	-e CGO_ENABLED="${CGO_ENABLED:-0}" \
	-v "$SRC":/src \
	-w /src $PORTS \
	"$IMAGE" go "$@"
