VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

.PHONY: build install clean test test-integration test-e2e test-all

build:
	go build -ldflags "-X main.version=$(VERSION)" -o heimdall-mcp ./cmd/heimdall-mcp

install:
	go install ./cmd/heimdall-mcp

clean:
	rm -f heimdall-mcp

# Default test target — unit tests only, race detector on.
# These are the 130+ tests that gate every PR. Fast, hermetic, no external
# deps. Build-tagged integration and e2e suites are NOT included.
test:
	go test ./... -race -count=1 -timeout 60s

# Layer 2 integration tests (T20). Exercises the real binary via os/exec
# against a fake Ollama httptest server. Build-tag gated — default `make test`
# does not run these. See internal/cli/integration_test.go.
test-integration:
	go test -tags=integration ./... -count=1 -timeout 300s

# Layer 3 e2e harness (T23). Runs the real `claude` CLI against hooks
# installed from a freshly built binary. Opt-in via HEIMDALL_E2E_CLAUDE=1
# AND the `claude` binary on PATH — otherwise tests cleanly t.Skip.
# See docs/plans/hooks/05-testing-rollout.md for what this proves.
test-e2e:
	go test -tags=e2e ./... -count=1 -timeout 300s

# Convenience: run unit + integration in sequence. e2e stays opt-in.
test-all: test test-integration
