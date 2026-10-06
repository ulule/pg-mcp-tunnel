.PHONY: build install test lint

build:
	go build -o pg-mcp-tunnel ./cmd/pg-mcp-tunnel

install:
	go install ./cmd/pg-mcp-tunnel

test:
	go test -race ./...

lint:
	golangci-lint run ./...
