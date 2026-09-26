# Integrations: sending to llm-hops, reading from it

Everything goes through one HTTP API on the server (`/api/openapi.json` describes it) and three doors that
other tools already know: OpenTelemetry in, Prometheus out, MCP for assistants.

## Sending spans

**Any language, with curl.** A span is JSON; post a list:

```bash
curl -s -X POST http://127.0.0.1:11602/api/v1/spans -H 'content-type: application/json' \
  -H 'authorization: Bearer TOKEN' \
  -d '[{"trace_id":"5f1c0a2b","span_id":"a1","service":"my-app","name":"request",
        "start_ms":1790409578748,"end_ms":1790409580000,"status":"ok",
        "attrs":{"client":"editor","model":"qwen3-coder-next","prompt_tokens":1200,"completion_tokens":80}}]'
```

**Python**: `pip install llm-hops` gives a `Tracer`, an ASGI middleware and `traceparent` helpers
([sdk/python](../sdk/python/README.md)).

**TypeScript or Node**: `fetch` and one object; see llm-harness's `src/hops.ts` for a 60-line emitter with a
timeout that never fails the request. A packaged SDK is roadmap M3.

**OpenTelemetry, any SDK, no code change if you already instrument.** Point the OTLP HTTP exporter at
`/v1/traces` with the JSON encoding:

```bash
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:11602/v1/traces
export OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/json
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer TOKEN"   # only with a token
export OTEL_SERVICE_NAME=my-agent
```

`service.name` becomes the service, span names the hops, and the GenAI semantic conventions map to the
page's names (`gen_ai.request.model` to `model`, `gen_ai.usage.input_tokens` to `prompt_tokens`,
`gen_ai.usage.output_tokens` to `completion_tokens`, `gen_ai.system` to `provider`). Prompt and completion
attributes (`gen_ai.prompt`, `gen_ai.completion`, message bodies) are dropped on arrival. Protobuf encoding is
refused with a hint; JSON only for now. This makes LangChain, LlamaIndex, LiteLLM, vLLM and every
OpenTelemetry-instrumented framework a source.

**yllm-gateway's request log**: `llm-hops serve -tail requests.jsonl`, or `llm-hops tail FILE -to URL` from
another machine.

## Reading

**The API.** `GET /api/v1/stats`, `/api/v1/traces`, `/api/v1/traces/{id}`, `/api/v1/flow`, `/api/v1/facets`;
the live stream at `/api/v1/stream` (server-sent events, one per trace touched). All windows are
milliseconds since the epoch. The OpenAPI document at `/api/openapi.json` feeds any client generator.

**Prometheus and Grafana.** Scrape `/metrics`:

```yaml
scrape_configs:
  - job_name: llm-hops
    static_configs: [{targets: ["127.0.0.1:11602"]}]
    authorization: {type: Bearer, credentials: TOKEN}   # only with a token
```

Series: `llm_hops_requests_5m`, `llm_hops_errors_5m`, `llm_hops_model_switches_5m`,
`llm_hops_request_ms{quantile}`, `llm_hops_hop_ms{hop,quantile}`, `llm_hops_model_requests_5m{model}`,
`llm_hops_model_request_ms{model,quantile}`, `llm_hops_spans_received_total`, `llm_hops_spans_rejected_total`.

**MCP: ask an assistant.** `llm-hops mcp` is a Model Context Protocol server over stdio that answers from a
running llm-hops server, so Claude Code, Cursor or any MCP client can ask "how is the system doing?" or "why
was that request slow?":

```bash
claude mcp add llm-hops -- llm-hops mcp -to http://127.0.0.1:11602            # Claude Code
# Cursor, Windsurf, Continue, Zed: the same command in their MCP settings (stdio server)
```

With a token: `-token TOKEN` or `LLM_HOPS_TOKEN` in the environment. Tools:

| Tool | Answers |
|---|---|
| `hops_stats` | counts, error rate, p50/p95, model switches, time per hop, per model, local against cloud, for a window (`15m`, `1h`, `6h`, `24h`, `7d`) |
| `hops_slowest` | the slowest requests in a window with where their time went |
| `hops_traces` | the newest requests, filterable by model, client, service, status, slower than, free text |
| `hops_trace` | one request as a text waterfall with every hop and attribute |
| `hops_flow` | the services and the hops between them |

The tools return text, never prompts (the store has none), and they never write.

## Webhooks and exports

Alerts by webhook are roadmap M4; until then Prometheus alert rules on `/metrics` do the job. `llm-hops
export` writes every span as JSON lines for any other tool.
