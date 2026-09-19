.PHONY: build test test-integration lint fmt vet

build:
	go build -o bin/vacmcp ./cmd/vacmcp

# decision-7's Tier 1: every test that can say what it means without a real
# Zoekt and a real CBM. The ones that cannot are behind `//go:build
# integration` and run from the target below. The exception is the top-level
# integration/ package, which is not tagged — it is the release gate, and a
# gate that runs nothing when a tag is forgotten is worse than a slow one, so
# it stays visible to a plain `go test` and skips itself when the fixture has
# not been built.
test:
	go test ./...

# A script rather than a recipe, because what it adds is a lifecycle: a
# codebase-memory-mcp daemon this run may or may not own, cleaned up on
# success, failure and signal, without swallowing the status the tests
# produced. That does not fit in a Make recipe and stays readable in a file.
#
# It is worth having because every CBM `cli` call otherwise starts a throwaway
# daemon: 1357s for this suite without a resident one and 657s with. CI has
# warmed a daemon since .github/actions/prepare-engines started one, so until
# now a developer running the gate paid twice what the gate guarding the branch
# pays.
#
# The script keeps what this recipe always did: CBM_CACHE_DIR pointed at the
# fixture's own store rather than the developer's global
# ~/.cache/codebase-memory-mcp, and -timeout because go's own default is a
# ten-minute watchdog for hanging tests rather than a budget for a suite that
# drives real git, Zoekt and CBM.
test-integration:
	.github/test-integration.sh

# With the tag, because that is the build every file is in: nothing carries
# `!integration`, so the tagged build is the tag-free one plus the
# *_integration_test.go files, and linting it is the only pass that sees all of
# them. Linting the tag-free build too would report every helper a real-engine
# test is the sole caller of as unused, which is a true statement about a build
# nobody runs and a false one about the code.
lint:
	golangci-lint run --build-tags=integration

fmt:
	gofmt -l -w .

vet:
	go vet ./...
