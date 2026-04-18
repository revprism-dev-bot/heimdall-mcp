VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

.PHONY: build install clean test test-integration test-e2e test-all bench bench-test hooks-smoke

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

# Tiered-retrieval token-savings benchmark (TODO section 2 follow-up).
# Runs the 18 built-in queries against the local .heimdall_db/ with Ollama
# embeddings, prints a per-query + aggregate table, and reports the
# summary-then-expand saving vs full-detail output.
# The DB path is auto-detected from CWD (FindRepoRoot → .heimdall_db/<model>/).
# Override with DB=<path> to pin a specific index, e.g. `make bench DB=/tmp/foo`.
# See docs/plans/hooks/09-tiered-retrieval-benchmark.md for methodology.
bench:
	go run ./cmd/bench-retrieval $(if $(DB),--db=$(DB)) --top-k=10 --expand-rate=0.2

# Smoke test for the bench harness — build-tag gated so `make test` stays
# focused on product code. Runs the bench against a fixture index with
# stub embeddings and asserts saving > 0% vs full.
bench-test:
	go test -tags=bench -race -count=1 ./cmd/bench-retrieval/

# End-to-end hooks smoke — fires one synthesized Claude Code payload per
# installed hook, asserts exit codes + hooks.log stages, reports pass/fail.
# Runs offline against an in-process fake Ollama so it's safe to invoke
# without a local LLM. Acts as a dispatchable substitute for "reopen Claude
# Code and eyeball hooks.log" dogfooding. See
# docs/plans/hooks/05-testing-rollout.md §Smoke harness for the scope
# boundary vs. layers 1-3.
hooks-smoke: build
	./heimdall-mcp hooks smoke --fake-ollama
