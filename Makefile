GO ?= go
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)

.PHONY: help frontend build agent agent-arm64 monitor test test-frontend lint tidy ipxe live run clean
ARCH ?= amd64

GOARCH ?= amd64

help:
	@echo "Targets:"
	@echo "  build      - build controller binary into bin/ (embeds prebuilt webui assets)"
	@echo "  frontend   - build Vue webui from frontend/ into internal/webui/assets (requires node)"
	@echo "  agent      - build inventory agent into bin/agent-$(GOARCH) (default amd64; GOARCH=arm64 to cross-compile)"
	@echo "  agent-arm64 - convenience: build arm64 agent (same as 'make agent GOARCH=arm64')"
	@echo "  monitor    - build monitoring agent into bin/monitor-$(GOARCH) (implanted into installed systems)"
	@echo "  test     - run go + frontend tests (frontend skipped if node absent)"
	@echo "  test-frontend - run frontend unit tests only"
	@echo "  lint     - go vet over cmd/ and internal/"
	@echo "  tidy     - go mod tidy"
	@echo "  ipxe     - fetch iPXE binaries via scripts/fetch-ipxe.sh"
	@echo "  live     - build Debian live image via scripts/build-live.sh (depends on agent + monitor)"
	@echo "  run      - run controller with config.example.yaml (sudo)"
	@echo "  clean    - remove build artifacts"

# Vite build output is committed to git (internal/webui/assets), so the
# controller can be built on machines that only have Go. Run this target
# after changing anything under frontend/.
frontend:
	cd frontend && npm ci && npm run build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o bin/controller ./cmd/controller

# bin/agent (no suffix) stays amd64 for back-compat; arch-suffixed copies
# feed build-live.sh --arch.
agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -trimpath \
	  -ldflags='-s -w -X main.agentVersion=$(VERSION)' \
	  -o bin/agent-$(GOARCH) ./cmd/agent
	ln -sf agent-$(GOARCH) bin/agent

agent-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath \
	  -ldflags='-s -w -X main.agentVersion=$(VERSION)' \
	  -o bin/agent-arm64 ./cmd/agent

# The monitoring agent is a separate component: the installer copies it
# from the live image into the installed OS when the profile/binding asks
# for it (agent_installed). Same arch-suffix convention as the agent.
monitor:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -trimpath \
	  -ldflags='-s -w -X main.monitorVersion=$(VERSION)' \
	  -o bin/monitor-$(GOARCH) ./cmd/monitor
	ln -sf monitor-$(GOARCH) bin/monitor

test: test-go test-frontend

# Go tests only — what CI runs and what you want during backend work.
test-go:
	$(GO) test ./cmd/... ./internal/...

# Frontend tests; skipped with a notice when node is absent so `make test`
# stays usable on Go-only machines (CI runs the frontend job unconditionally).
test-frontend:
	@if command -v npm >/dev/null 2>&1; then \
		cd frontend && npm test; \
	else \
		echo "test-frontend: npm not found — skipping (run 'cd frontend && npm test' on a machine with node)"; \
	fi

lint:
	$(GO) vet ./cmd/... ./internal/...

tidy:
	$(GO) mod tidy

# script added by sub-D
ipxe:
	./scripts/fetch-ipxe.sh

# live image embeds the agent and monitor binaries, so both targets must run
# first. ARCH=amd64 (default) or arm64; the binaries are cross-compiled to match.
live: agent monitor
	./scripts/build-live.sh --arch $(ARCH)

run:
	sudo ./bin/controller -config config.example.yaml

clean:
	rm -rf bin/ boot/ live-image/binary live-image/cache live-image/.build live-image/chroot live-image/auto/local
