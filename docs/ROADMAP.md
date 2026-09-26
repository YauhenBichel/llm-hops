# Roadmap: from one box to anyone's LLM system

Where llm-hops stands on 26 September 2026: version 0.1.0 runs on one server, fed by one adapter (the
yllm-gateway request log), read by one person. This is the plan to make it mature and usable in other
people's systems. Each milestone has a goal, tasks, and the test that says it is done. Tasks are GitHub
issues with the milestone's label; the order inside a milestone is the suggested order.

The rule for every task: keep the shape. One binary, one SQLite file, one page with no build step, no prompt
text stored, a server that is down costs a sender nothing.

## M1. Trustworthy on one box (October, week 1)

**Goal:** a person can run it for a month and trust every number on the page.

1. **Time in the adapter.** yllm-gateway's log has second-resolution timestamps; ask the gateway for
   millisecond `ts` (a one-line change there) and read either. Done when a trace's start is exact to the
   millisecond for new lines and the old lines still load.
2. **A configuration file** (`-config hops.toml`): listen address, database, retention, the logs to follow,
   an optional bearer token for `POST /api/v1/spans` and the page. Done when the systemd unit needs no flags.
3. **Retention that keeps the picture.** Prune spans by age but keep per-day statistics (requests, errors,
   p50, p95, per model) in a small table, so the stats view can show 90 days while spans keep 14. Done when
   a 90-day window answers in under a second on a million-span database.
4. **A Prometheus endpoint** (`/metrics`): requests, errors, p50/p95 per hop and per model for the last
   5 minutes, spans received, spans rejected. Done when Grafana draws it.
5. **Load test and limits.** 10,000 spans a second for ten minutes on a laptop: no lost spans, memory under
   100 MB, the page still answers. Document the numbers. Done when the numbers are in the README.
6. **Backup and restore.** `llm-hops export -since -until` writes JSON lines; `llm-hops import` reads them.
   Done when a database can be rebuilt from an export byte for byte.

## M2. Anyone's system: adapters and OpenTelemetry (October, weeks 2 to 3)

**Goal:** a person with Ollama, llama.cpp, vLLM or an OpenTelemetry-instrumented service sees their first
picture within ten minutes, with no code change.

1. **OTLP/JSON receiver** (`POST /v1/traces`, the OpenTelemetry protocol over HTTP with JSON): map resource
   `service.name` to `service`, span name to `name`, attributes to attrs, status to status. Done when the
   OpenTelemetry Python SDK's example exporter shows traces on the page. This one task makes every
   instrumented LLM framework (LangChain, LlamaIndex, LiteLLM, vLLM's tracing) a source.
2. **Ollama server log adapter**: model loads and unloads, the runner's start, request lines with their
   durations. Done when a model switch appears as a `load` hop with its size.
3. **llama.cpp server adapter**: the server's request log with prompt and generation timings. Done when a
   trace has `prefill` and `generate` from real timings.
4. **OpenAI-compatible proxy mode**: `llm-hops proxy -upstream http://127.0.0.1:11434` sits in front of any
   OpenAI-compatible server, forwards every request unchanged, and makes a trace with queue, time to first
   token and total from what it observes. Done when a user with only Ollama gets traces by changing one
   base URL. This is the adapter that needs no log at all.
5. **An adapter guide** (`docs/adapters.md`): the function, the test with three real lines, the follower;
   how to submit one. Done when an outside contributor has submitted one.

## M3. SDKs (November, week 1)

**Goal:** a trace crosses machines and languages.

1. **TypeScript SDK** (`sdk/ts`, zero dependencies): `Tracer`, `trace`, `child`, `traceparent`, a fetch
   wrapper that propagates the header; a Node middleware for Express and Hono. Done when llm-harness on the
   laptop starts a trace that the gateway on the server continues.
2. **Go SDK** (`sdk/go`): the same surface, plus `net/http` middleware. Done when llm-hops traces itself.
3. **Python SDK on PyPI**, with a release workflow. Done when `pip install llm-hops` works from a clean
   machine.
4. **Semantic conventions**: one page naming the attributes and hop names the page understands
   (`model`, `prompt_tokens`, `ttft_ms`, `cache: hit|lost`, `load`, `prefill`, `generate`), aligned with
   OpenTelemetry's GenAI conventions where they exist. Done when the OTLP receiver maps `gen_ai.*`
   attributes to them.

## M4. The page (November, weeks 2 to 3)

**Goal:** the page answers the questions a person has after the first day.

1. **Compare two windows**: yesterday against today, before and after a change; the stats side by side.
2. **Model timeline**: which model was loaded when, as a band across the top of the traces view; loads and
   evictions from the adapters. This is the picture of "who else is using the GPU".
3. **Cache view**: prefill time against prompt size, coloured by cache hit or lost; the share of turns that
   lost the cache and what it cost.
4. **Per-client view**: one client's requests, the models it used, its p95, its errors.
5. **Alerts**: a threshold on p95, error rate or model switches per hour, delivered by a webhook (ntfy, Slack,
   anything with a URL). Done when a webhook fires in a test.
6. **Export**: the current table as CSV; a trace as JSON; a stats window as JSON.
7. **Accessibility pass**: keyboard on the flow map, screen-reader text for the waterfall, contrast checked
   in both themes.

## M5. Inside the gateway (with M3)

**Goal:** one trace from the laptop to the token, with the hops inside the gateway.

1. yllm-gateway: the SDK's middleware, a trace id in `observed()`, spans for resolve, memory augmentation,
   backend start, queue, the prompt check and its hidden probe, upstream with the first token, the reply.
2. llm-harness: a trace started on the laptop and the `traceparent` header sent to the gateway.
3. agent-harness: a span per decision, with the cache-lost verdict, linked to the gateway's trace.
Done when the waterfall of one editor request shows the router's decision, the gateway's queue and the
model's prefill in one picture.

## M6. Distribution (December)

**Goal:** installing takes one command on every platform.

1. GoReleaser: release binaries and checksums from a tag, for Linux, macOS and Windows, amd64 and arm64.
2. A Docker image (`ghcr.io/yauhenbichel/llm-hops`), distroless, with a volume for the database.
3. A Homebrew tap. A Debian package if asked.
4. `llm-hops install-service`: writes and enables the systemd user unit (or launchd on macOS).
5. Versioned API (`/api/v1` stays; changes go to `/api/v2`) and a CHANGELOG.

## M7. Community (from M2 on)

1. Issue templates: a bug, an adapter request, "a report from a different system" with a screenshot.
2. A docs site from `docs/` (GitHub Pages), with a live demo page fed by the synthetic generator.
3. A demo recording (GIF) in the README.
4. A comparison page: what llm-hops is against Langfuse, Phoenix, Jaeger and Grafana Tempo, honestly:
   smaller, single-binary, local-first, no prompt storage, not a prompt-evaluation tool.
5. Respond to every issue within a week; a first contributor's pull request merged within a week.

## Out of scope, on purpose

Storing prompts and completions; user accounts and roles (put a proxy in front); a hosted service; a
JavaScript build chain for the page; a second database engine.

## Order

M1 first: nobody adopts a tool whose numbers they doubt. M2's OTLP receiver and proxy mode are the two
tasks that open the door to other systems; do them before anything in M4. M5 runs beside M3 because it is
the author's own system and the best test of the SDKs. M6 when the API has not changed for a month.
