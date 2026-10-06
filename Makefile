.PHONY: build install test

build:
	go build -o pg-mcp-tunnel ./cmd/pg-mcp-tunnel

install:
	go install ./cmd/pg-mcp-tunnel

test:
	go test -race ./...
