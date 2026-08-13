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

# -timeout because go's own default is 10 minutes, which is a watchdog for
# tests that hang rather than a budget: these drive real git, Zoekt and CBM
# subprocesses, and cmd/vacmcp alone spends longer than that building and
# tearing down the repositories, indexes and graphs it asserts against. The tag
# adds the real-engine tests to the run rather than replacing anything, so this
# is every test in the module; it needs testdata/prepare-fixture.sh to have run.
test-integration:
	go test -tags=integration -timeout 30m ./...

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
