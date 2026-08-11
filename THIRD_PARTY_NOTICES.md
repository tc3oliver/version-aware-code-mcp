# Third Party Notices

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE).

## Bundled dependencies

These modules are declared in `go.mod` and linked into the `vacmcp` binary.

| Module | Version | License |
| --- | --- | --- |
| [go.yaml.in/yaml/v4](https://github.com/yaml/go-yaml) | v4.0.0-rc.6 | Apache-2.0, with MIT for the files ported from libyaml |

The YAML parser is Apache-2.0 (Copyright 2011-2019 Canonical Ltd, Copyright 2025
The go-yaml Project Contributors). Its `internal/libyaml` files were ported from
the C libyaml and remain under the original MIT license (Copyright 2006-2010
Kirill Simonov); see the module's `NOTICE` file for the file list.

## External services

These are not distributed with this project. You install and run them yourself;
they are listed because the server is designed to query them.

| Component | License | Project |
| --- | --- | --- |
| Zoekt | Apache-2.0 | https://github.com/sourcegraph/zoekt |
| codebase-memory-mcp (CBM) | see upstream | https://github.com/DeusData/codebase-memory-mcp |
