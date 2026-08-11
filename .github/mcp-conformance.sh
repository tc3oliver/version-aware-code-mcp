#!/usr/bin/env bash
# doc-1's protocol layer, checked with the reference client: MCP Inspector's CLI
# drives the built vacmcp binary over STDIO and calls all four tools for real.
#
# Real is the point. An in-process test can register a tool and call it without
# ever proving that a third-party MCP client can discover and invoke it, and the
# protocol version is exactly where that goes wrong: the Inspector reaches
# 2026-07-28 only when the server is asked with "protocolEra": "modern", which
# makes it probe `server/discover` instead of the legacy `initialize` handshake.
# So the negotiated version is asserted, not assumed — a silent fall back to
# 2025-11-25 would otherwise pass this script while failing doc-1.
#
# Needs the fixture (testdata/prepare-fixture.sh), a Zoekt web server, which it
# starts itself, and codebase-memory-mcp reachable at the command the fixture
# configuration names.
set -euo pipefail

inspector=@modelcontextprotocol/inspector@2.1.0

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
config="$root/testdata/fixture/config.yaml"
index="$root/testdata/fixture/zoekt-index"

if [ ! -f "$config" ]; then
	echo "mcp-conformance: $config missing, run testdata/prepare-fixture.sh" >&2
	exit 1
fi

work=$(mktemp -d)
zoekt_pid=
cleanup() {
	[ -n "$zoekt_pid" ] && kill "$zoekt_pid" 2>/dev/null
	rm -rf "$work"
}
trap cleanup EXIT

# The fixture configuration names the Zoekt URL, so the server is started on
# that address rather than on a second copy of the number.
zoekt_url=$(sed -n 's|^[[:space:]]*url:[[:space:]]*||p' "$config" | head -1)
zoekt-webserver -index "$index" -listen "${zoekt_url#http://}" -rpc -html=false >"$work/zoekt.log" 2>&1 &
zoekt_pid=$!
for _ in $(seq 1 150); do
	curl -fs -o /dev/null "$zoekt_url/healthz" && break
	sleep 0.2
done
if ! curl -fsS -o /dev/null "$zoekt_url/healthz"; then
	echo "mcp-conformance: zoekt-webserver at $zoekt_url never became ready" >&2
	cat "$work/zoekt.log" >&2
	exit 1
fi

go build -o "$work/vacmcp" "$root/cmd/vacmcp"

# "protocolEra": "modern" is what makes the Inspector probe server/discover.
cat >"$work/inspector.json" <<EOF
{
  "mcpServers": {
    "vacmcp": {
      "command": "$work/vacmcp",
      "args": ["serve", "--stdio", "--config", "$config"],
      "protocolEra": "modern"
    }
  }
}
EOF

# One Inspector run per call, each captured for the assertions below. Failures
# come back as a JSON error object with a zero exit status, so the output is
# what decides, not $?.
inspect() {
	local name=$1
	shift
	echo "mcp-conformance: $name"
	npx --yes "$inspector" --cli \
		--config "$work/inspector.json" --server vacmcp --format json "$@" \
		>"$work/$name.json"
	cat "$work/$name.json"
}

inspect discover --method initialize
inspect tools_list --method tools/list
inspect list_contexts --method tools/call --tool-name list_contexts
inspect search_code --method tools/call --tool-name search_code \
	--tool-arg context=demo-v2 query=NewHandler
inspect trace_calls --method tools/call --tool-name trace_calls \
	--tool-arg context=demo-v2 symbol=Process direction=callees depth=3
inspect get_code --method tools/call --tool-name get_code \
	--tool-arg context=demo-v2 path=processor.go start_line=1 end_line=6

# Every call is checked for what it was supposed to prove: that the answer came
# back over 2026-07-28, and that it is release/v2's answer. A tool that returned
# v1's handler, or no handler, is the version leak this project exists to stop,
# and it would otherwise look like a successful invocation.
python3 - "$work" <<'PY'
import json, pathlib, sys

work = pathlib.Path(sys.argv[1])
failures = []


def result(name):
    payload = json.loads((work / f"{name}.json").read_text())
    if "result" not in payload:
        failures.append(f"{name}: {json.dumps(payload)}")
        return None
    return payload["result"]


def check(name, condition, want):
    if not condition:
        failures.append(f"{name}: {want}")


def structured(name):
    res = result(name)
    return res.get("structuredContent") if res else None


discover = result("discover")
if discover:
    check(
        "discover",
        discover.get("protocolVersion") == "2026-07-28",
        f"protocol version is {discover.get('protocolVersion')!r}, want '2026-07-28'"
        " — the Inspector fell back to the legacy initialize handshake",
    )

tools = result("tools_list")
if tools:
    got = sorted(t["name"] for t in tools["tools"])
    want = ["get_code", "list_contexts", "search_code", "trace_calls"]
    check("tools/list", got == want, f"tools are {got}, want {want}")

contexts = structured("list_contexts")
if contexts:
    got = sorted(c["id"] for c in contexts["contexts"])
    check("list_contexts", got == ["demo-v1", "demo-v2"], f"contexts are {got}")

search = structured("search_code")
if search:
    check(
        "search_code",
        search["context"]["branch"] == "release/v2" and search["matches"],
        f"{len(search['matches'])} matches on branch"
        f" {search['context']['branch']!r}, want NewHandler on 'release/v2'",
    )

trace = structured("trace_calls")
if trace:
    calls = [(c["caller"], c["callee"]) for c in trace["calls"]]
    check(
        "trace_calls",
        ("Process", "NewHandler") in calls,
        f"calls are {calls}, want Process -> NewHandler (v2's handler)",
    )

code = structured("get_code")
if code:
    check(
        "get_code",
        "NewHandler" in code["content"]
        and code["context"]["branch"] == "release/v2",
        f"read {code['path']} at {code['context']['branch']!r}, content does not"
        " carry v2's handler",
    )

if failures:
    print("\nmcp-conformance: FAILED", file=sys.stderr)
    for failure in failures:
        print(f"  {failure}", file=sys.stderr)
    sys.exit(1)

print("\nmcp-conformance: 2026-07-28 negotiated by server/discover; "
      "list_contexts, search_code, trace_calls and get_code all invoked")
PY
