# Third Party Notices

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE).

## Bundled dependencies

These modules are linked into the `vacmcp` binary. `.github/license-check.sh`
compares this table against `go list -deps ./cmd/vacmcp` on every pull request,
so a dependency added, removed or upgraded without updating this table fails CI.

| Module | Version | License |
| --- | --- | --- |
| [github.com/google/jsonschema-go](https://github.com/google/jsonschema-go) | v0.4.3 | MIT |
| [github.com/modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) | v1.7.0 | Apache-2.0, with MIT for the contributions not yet relicensed |
| [github.com/segmentio/asm](https://github.com/segmentio/asm) | v1.1.3 | MIT |
| [github.com/segmentio/encoding](https://github.com/segmentio/encoding) | v0.5.4 | MIT |
| [github.com/yosida95/uritemplate/v3](https://github.com/yosida95/uritemplate) | v3.0.2 | BSD-3-Clause |
| [go.yaml.in/yaml/v4](https://github.com/yaml/go-yaml) | v4.0.0-rc.6 | Apache-2.0, with MIT for the files ported from libyaml |
| [golang.org/x/oauth2](https://github.com/golang/oauth2) | v0.35.0 | BSD-3-Clause |
| [golang.org/x/sync](https://github.com/golang/sync) | v0.20.0 | BSD-3-Clause |
| [golang.org/x/sys](https://github.com/golang/sys) | v0.41.0 | BSD-3-Clause |
| [golang.org/x/time](https://github.com/golang/time) | v0.15.0 | BSD-3-Clause |

The YAML parser is Apache-2.0 (Copyright 2011-2019 Canonical Ltd, Copyright 2025
The go-yaml Project Contributors). Its `internal/libyaml` files were ported from
the C libyaml and remain under the original MIT license (Copyright 2006-2010
Kirill Simonov); see the module's `NOTICE` file for the file list.

The MCP Go SDK is mid-transition from MIT to Apache-2.0: contributions whose
authors have consented to relicensing are Apache-2.0, the rest remain MIT. See
the module's `LICENSE` file.

The `golang.org/x` modules are BSD-3-Clause (Copyright 2009 The Go Authors), as
is `github.com/yosida95/uritemplate/v3` (Copyright 2016 Kohei YOSHIDA).

## External services

These are not distributed with this project. You install and run them yourself;
they are listed because the server is designed to query them.

| Component | License | Project |
| --- | --- | --- |
| Zoekt | Apache-2.0 | https://github.com/sourcegraph/zoekt |
| codebase-memory-mcp (CBM) | see upstream | https://github.com/DeusData/codebase-memory-mcp |
