# Contributing

## Requirements

- Go 1.26 or newer
- [golangci-lint](https://golangci-lint.run/) 2.x

## Build, test, lint

```bash
make build   # go build -o bin/vacmcp ./cmd/vacmcp
make test    # go test ./...
make lint    # golangci-lint run
make fmt     # gofmt -l -w .
make vet     # go vet ./...
```

Run `make lint` and `make test` before opening a pull request.

## Repository layout

Packages are added as the code that belongs in them lands, so not every
directory below exists yet.

```text
cmd/vacmcp/     binary entry point
server/         MCP server and transports
context/        CodeContext type, resolver, config loading
provider/       SearchProvider / GraphProvider / SourceProvider interfaces
adapters/       provider implementations (zoekt, cbm, git)
tools/          MCP tool handlers
evidence/       evidence and location types
config/         example configuration
testdata/       test fixtures
integration/    integration tests
docker/         Docker Compose setup
```

Exported packages live in the root package hierarchy; implementation that must
not be imported from outside this module goes under `internal/`.

## Versioned demo repository

`testdata/versioned-demo-repo/` is the fixture the version-correctness checks
run against: a git repository with three branches whose `Process()` calls a
different handler per release.

| Branch | `Process()` calls |
| --- | --- |
| `main` | `LegacyHandler()` |
| `release/v1` | `LegacyHandler()` |
| `release/v2` | `NewHandler()` |

It is generated, never committed — a nested `.git` directory cannot be tracked
by this repository, so the path is in `.gitignore`:

```bash
./testdata/gen-versioned-demo-repo.sh
```

The script rebuilds the fixture from scratch on every run and pins the commit
identity and dates, so repeated runs produce the same commit hashes and an
already indexed fixture stays valid. Go tests reach it through
`internal/demorepo`, which generates it on demand and resolves each branch's
revision with `git rev-parse`; never hard-code a commit hash.

## Pull requests

- Keep a pull request to one reviewable change.
- Include tests for behavior changes.
- Update documentation affected by the change in the same pull request.

## License

Contributions are accepted under the Apache License 2.0.
