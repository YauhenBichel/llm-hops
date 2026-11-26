# llm-hops

**See every hop of a request through your local LLM system.**

A request to a local model rarely takes one step. It passes a router that decides whether it may leave the
machine, a gateway that picks the model and checks the prompt, a queue that waits for the one slot, a model
server that may have to load 50 GB first, then prefill, generation and the reply. When an answer takes 40
seconds instead of one, the question is always the same: *where did the time go?*

llm-hops answers it. One static binary keeps the traces in a SQLite file and serves a page with three views:

- **Flow**: the services in your system and the hops between them, with the live requests travelling along
  the edges as they happen, and the newest requests as chips you can open.
- **Traces**: every request in the window, filterable by model, client, service, status and duration, with
  queue time, time to first token, total, tokens and tokens per second. Click one for its **waterfall**: each
  hop as a bar on a time axis, the first-token marker, and every attribute.
- **Stats**: requests per minute, error rate, percentiles per hop ("where the time goes"), per-model rows
  and the number of model switches.

Light and dark. Keyboard: `1` `2` `3` switch views, `/` searches, `j` `k` walk the traces, `Esc` closes,
space pauses the live stream. No build step, no external requests from the page, no prompt text stored.

![the flow view: services as boxes, hops as edges, live requests as dots](docs/img/flow.png)

![a trace: the waterfall of its hops, first-token marker, every attribute](docs/img/trace.png)

## Try it in one minute

```bash
go install github.com/YauhenBichel/llm-hops/cmd/llm-hops@latest   # or download a binary from the releases
llm-hops serve -demo 300 -live
```

With Homebrew, on macOS or Linux (available once the formula is in
[the tap](https://github.com/YauhenBichel/homebrew-tap); until then use `go install` or a release binary):

```bash
brew install yauhenbichel/tap/llm-hops
```

Open http://127.0.0.1:11602/. The demo is a small home LLM system: a router on a laptop, a gateway with one
slot, a model server that swaps models and loses its cache, five clients.

## Feed it your system

**From a log you already have.** yllm-gateway writes one JSON line per request with queue, upstream,
first-token and total times. Follow it and every line becomes a trace, no code change:

```bash
llm-hops serve -tail ~/yllm-gateway/var/requests.jsonl -from-start
```

Rotation is survived, a partial line waits for its newline, and the same line never makes two traces.

**Model loads from Ollama's own log.** On the machine that runs Ollama, `-ollama-journal` (or `ollama_journal
= true` in the file) follows `journalctl -u ollama`: every "loading model via llama-server" ... "started in
9.8 seconds" pair becomes a trace of its own, service `ollama`, name `load`, with the model's name (resolved
through Ollama's API), the blob and the seconds. The stats count loads and time loading apart from requests.
`ollama_since = "7 days ago"` reads the history first. This is the picture of "who else is using the GPU".
`llm-hops import FILE` loads a file once; `llm-hops tail FILE -to URL` follows it from another machine.

**From your code.** Spans are JSON; post a list to `POST /api/v1/spans`:

```json
{"trace_id": "5f1c…", "span_id": "a1", "parent_id": null, "service": "gateway", "name": "request",
 "start_ms": 1790409578748, "end_ms": 1790409624115, "status": "ok",
 "attrs": {"client": "editor", "model": "qwen3-coder-next", "queue_ms": 1200, "ttft_ms": 5564}}
```

The Python SDK (`pip install llm-hops`, standard library only) opens spans with a context manager, posts
them in batches from a background thread, and has a pure ASGI middleware that makes one span per request
and passes the trace id in the W3C `traceparent` header: [sdk/python](sdk/python/README.md).

A span has a `service`, a `name`, a start and an end in milliseconds, a `status` and flat attributes. The
names the page draws with meaning: `decide`, `check`, `queue`, `load`, `prefill`, `generate`, `upstream`,
`reply`. Any other name is drawn as a plain hop.

## Integrations

Three doors other tools already know, all in [docs/integrations.md](docs/integrations.md):

- **OpenTelemetry in**: point any OTel SDK's OTLP HTTP exporter (JSON encoding) at `/v1/traces`; GenAI
  attributes map to the page's names, prompt text is dropped on arrival.
- **Prometheus out**: `/metrics`, the last five minutes per hop and per model.
- **MCP for assistants**: `claude mcp add llm-hops -- llm-hops mcp -to http://127.0.0.1:11602` gives Claude
  Code, Cursor or any MCP client five read-only tools: stats, the slowest requests, a filtered list, one
  request as a text waterfall, the flow.

The HTTP API is described at `/api/openapi.json`.

## Use with other MCP clients (Grok Build, Cursor, any MCP client)

`llm-hops mcp -to <server-url>` speaks MCP over stdio and reads from a running `llm-hops serve`. Any client that
starts a stdio server can use it. In `.mcp.json`, `.cursor/mcp.json` or the client's own server list:

```json
{
  "mcpServers": {
    "llm-hops": {
      "command": "llm-hops",
      "args": ["mcp", "-to", "http://127.0.0.1:11602"],
      "env": { "LLM_HOPS_TOKEN": "<your-token>" }
    }
  }
}
```

Leave `env` out when the server has no `token`. Use the full path to `llm-hops` if the client does not see your `PATH`.

Grok Build (xAI) reads `~/.grok/config.toml`, or `.grok/config.toml` in a project:

```toml
[mcp_servers.llm-hops]
command = "llm-hops"
args = ["mcp", "-to", "http://127.0.0.1:11602"]
env = { LLM_HOPS_TOKEN = "<your-token>" }
```

or in one line: `grok mcp add llm-hops -- llm-hops mcp -to http://127.0.0.1:11602`. Grok Build also reads `.mcp.json`
and `.cursor/mcp.json`, so the JSON above works there too.

Models without MCP support (for example Meta's Muse Spark, whose API offers function calling) can use these tools
through an MCP-to-function-calling bridge; llm-harness provides one, see llm-harness's README.

Tested with Claude Code. The Grok Build block follows xAI's documentation and has not been run.

## The API

| Call | Gives |
|---|---|
| `POST /api/v1/spans` | a list of spans, or `{"spans": [...]}`; answers accepted, rejected, the trace ids touched |
| `GET /api/v1/traces?since&until&limit&offset&sort&count&model&client&service&status&min_ms&q` | trace summaries, newest first; `sort=duration` for slowest first, `count=1` adds the `total` that match |
| `GET /api/v1/traces/{id}?max_spans` | one trace: summary and every span, or its first `max_spans` |
| `GET /api/v1/stats?since&until&bucket_ms` | counts, p50/p95/max, per-hop and per-model rows, requests per bucket, model switches |
| `GET /api/v1/flow?since&until` | the services and the aggregated hops between them |
| `GET /api/v1/facets?since&until` | the values the filters can take |
| `GET /api/v1/stream` | server-sent events: one per trace touched |
| `GET /api/v1/health` | ok, time, database path, uptime |

| `GET /metrics` | Prometheus text: the last five minutes, per hop and per model |

Times are milliseconds since the epoch. Spans older than `keep_days` (14) are rolled up into daily statistics
per model and dropped, at the start and every ten minutes; the stats view shows rolled-up days too, marked approximate.

Limits, so one sender cannot hurt the rest: an id is at most 64 bytes and a longer one is rejected, not
shortened; a span starts no more than a day ahead and lasts at most 24 hours; a trace holds at most
`max_spans_per_trace` spans (5000; more are dropped, and the answer and `llm_hops_spans_dropped_total` say so).
The answer to a `POST` gives the reasons for what it rejected. A span sent again with the same trace and span
id replaces the earlier one. The details are in [docs/deploy.md](docs/deploy.md#limits-and-the-size-of-the-file).

## Running it for real

`llm-hops serve` listens on loopback by default; `llm-hops config` prints a configuration file to start from.
A `token` guards the API and the page when you need more than loopback (`Authorization: Bearer` for senders,
`/#token=…` once for a browser). `llm-hops export` and `import` move a store between machines. A systemd unit and the setup used on the
author's server are in [docs/deploy.md](docs/deploy.md). The SQLite file is written with WAL and
`synchronous=FULL`, so a power cut loses no span that was acknowledged.

What is **not** stored: prompts, completions, keys. What the page shows is what the spans carry, so the rule
is on the sender: [docs/privacy.md](docs/privacy.md).

## Numbers

Measured on 26 September 2026 on a laptop (Apple M-series), `llm-hops bench -rate 10000 -seconds 10` against a
fresh server on the same machine:

| | |
|---|---|
| spans sent | 100,318 in 100 posts over 10.1 s (9,935 spans/s) |
| accepted | 100,318; no post failed; the slowest post took 54 ms |
| server memory after | 33 MB resident |
| the page during the load | answered in 0.5 ms |
| statistics over everything (13,742 traces, 100k spans) | 561 ms |
| database | 30 MB |

## Building

```bash
go build ./cmd/llm-hops           # Go 1.23+, no CGO, the page is embedded
go test -race ./...
cd sdk/python && uv venv && uv pip install -e ".[dev]" && .venv/bin/python -m pytest
```

## Why this exists

The author runs a home LLM server for coding tools and agents. Its worst days were explained only after the
fact, by reading a request log line by line: a queue that waited for the wrong model, a cache lost by a
program that unloaded the model once a minute, a prompt that was cut silently. The numbers were there; the
picture was not. llm-hops is the picture. The system it was built for is described in
[yserver-local-llm-system](https://github.com/YauhenBichel/yserver-local-llm-system).

## Where it is going

Seven milestones, each task with its "done when", in [docs/ROADMAP.md](docs/ROADMAP.md); the tasks are the
issues with a milestone label. The two that open the door to other systems come first: an OpenTelemetry
receiver and a proxy mode that needs no log at all.

## Licence

Apache-2.0. Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
