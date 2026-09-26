.PHONY: build test lint dist demo sdk-test

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -ldflags="$(LDFLAGS)" -o llm-hops ./cmd/llm-hops

test:
	go test -race -count=1 ./...

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"

dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/llm-hops-linux-amd64 ./cmd/llm-hops
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/llm-hops-linux-arm64 ./cmd/llm-hops
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/llm-hops-darwin-arm64 ./cmd/llm-hops

demo: build
	./llm-hops serve -db /tmp/hops-demo.db -demo 300 -live

sdk-test:
	cd sdk/python && uv venv -q && uv pip install -q -e ".[dev]" && .venv/bin/python -m ruff check . && .venv/bin/python -m mypy --strict llm_hops tests && .venv/bin/python -m pytest -q
