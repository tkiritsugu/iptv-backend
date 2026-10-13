# All Go commands run in Docker (golang:1.26) so the host toolchain is not used.
GO := ./hack/go.sh

.PHONY: help build test race vet fmt tidy run smoke clean

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-8s %s\n", $$1, $$2}'

build: ## Compile ./cmd/iptv into bin/iptv
	$(GO) build -o bin/iptv ./cmd/iptv

test: ## Run the tests
	$(GO) test ./...

race: ## Run the tests with the race detector
	CGO_ENABLED=1 $(GO) test -race ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format all Go files
	$(GO) fmt ./...

tidy: ## Tidy go.mod/go.sum
	$(GO) mod tidy

run: ## Start the service on :8080 with demo data
	$(GO) run ./cmd/iptv

smoke: build ## Start with docker compose, check the API, then stop
	docker compose up --build -d
	@sleep 2; ./hack/smoke.sh; status=$$?; docker compose logs --no-color iptv | tail -40; docker compose down; exit $$status

clean: ## Remove build and cache artifacts
	rm -rf bin .devcache data
