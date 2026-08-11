.PHONY: build test lint fmt vet

build:
	go build -o bin/vacmcp ./cmd/vacmcp

test:
	go test ./...

lint:
	golangci-lint run

fmt:
	gofmt -l -w .

vet:
	go vet ./...
