GO ?= go
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)

.PHONY: help build agent test tidy ipxe live run clean

help:
	@echo "Targets:"
	@echo "  build  - build controller binary into bin/"
	@echo "  agent  - build inventory agent binary into bin/agent (Linux/amd64, static)"
	@echo "  test   - run go tests"
	@echo "  tidy   - go mod tidy"
	@echo "  ipxe   - fetch iPXE binaries via scripts/fetch-ipxe.sh"
	@echo "  live   - build Debian live image via scripts/build-live.sh (depends on agent)"
	@echo "  run    - run controller with config.example.yaml (sudo)"
	@echo "  clean  - remove build artifacts"

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o bin/controller ./cmd/controller

agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath \
	  -ldflags='-s -w -X main.agentVersion=$(VERSION)' \
	  -o bin/agent ./cmd/agent

test:
	$(GO) test ./cmd/... ./internal/...

tidy:
	$(GO) mod tidy

# script added by sub-D
ipxe:
	./scripts/fetch-ipxe.sh

# live image embeds the agent binary, so the agent target must run first.
live: agent
	./scripts/build-live.sh

run:
	sudo ./bin/controller -config config.example.yaml

clean:
	rm -rf bin/ boot/ live-image/binary live-image/cache live-image/.build live-image/chroot live-image/auto/local
