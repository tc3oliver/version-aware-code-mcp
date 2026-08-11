# Security Policy

## Supported Versions

This project is pre-1.0. Only the latest released version receives security
fixes.

## Reporting a Vulnerability

Report vulnerabilities privately to **tc3oliver@gmail.com**. Please do not open
a public issue for an unfixed vulnerability.

Include what you have: affected version, reproduction steps, and impact.

Response targets:

| Stage | Target |
| --- | --- |
| Acknowledge receipt | within 5 working days |
| Initial assessment | within 10 working days |
| Fix or mitigation plan | communicated after assessment |

Once a fix is released, the report is credited in the release notes unless you
ask otherwise.

## Scope

This server reads local repositories and queries local search and graph
providers. It does not require an LLM API key, does not compute embeddings, and
does not upload source code on its own. The MCP client you connect it to is
chosen and trusted by you, and is outside this project's control.
