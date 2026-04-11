VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

.PHONY: build install clean test

build:
	go build -ldflags "-X main.version=$(VERSION)" -o heimdall-mcp ./cmd/heimdall-mcp

install:
	go install ./cmd/heimdall-mcp

clean:
	rm -f heimdall-mcp

test:
	go test ./... -race -count=1 -timeout 60s
