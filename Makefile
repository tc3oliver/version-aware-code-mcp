.PHONY: build test lint fmt vet

build:
	go build -o bin/vacmcp ./cmd/vacmcp

# -timeout because go's own default is 10 minutes, which is a watchdog for
# tests that hang rather than a budget: these drive real git, Zoekt and CBM
# subprocesses, and cmd/vacmcp alone spends longer than that building and
# tearing down the repositories, indexes and graphs it asserts against.
test:
	go test -timeout 30m ./...

lint:
	golangci-lint run

fmt:
	gofmt -l -w .

vet:
	go vet ./...
