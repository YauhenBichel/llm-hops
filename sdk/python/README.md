# llm-hops, the Python SDK

Open spans in your LLM service and post them to a [llm-hops](https://github.com/YauhenBichel/llm-hops) server.
Standard library only. A server that is down costs you a warning, never a blocked request.

```bash
pip install llm-hops
```

## Spans by hand

```python
from llm_hops import Tracer

tracer = Tracer("http://127.0.0.1:11602", service="gateway")

with tracer.trace("request", attrs={"client": ip, "model": model}) as root:
    with root.child("queue"):
        lease = await queue.acquire(model)
    with root.child("upstream", service="ollama", attrs={"model": model}) as up:
        reply = await call_model(...)
        up.attrs["ttft_ms"] = reply.ttft_ms
        up.attrs["completion_tokens"] = reply.completion_tokens
```

Spans are queued and posted in batches from a background thread. `tracer.close()` flushes at shutdown.
`Tracer(None)` keeps spans in memory (`tracer.drain()`), for tests.

## One span per request, automatically

A pure ASGI middleware, so streaming replies and client disconnects keep working:

```python
from llm_hops.asgi import HopsMiddleware

app.add_middleware(HopsMiddleware, tracer=tracer)

# inside a handler
span = request.state.hops_span
with span.child("check", attrs={"prompt_tokens": n}):
    ...
```

The request's span ends when the response body ends, so a streamed reply is measured to its last byte.

## Traces across services

The trace id travels in the W3C `traceparent` header. The middleware reads it from the request and returns it
in the response. To continue a trace by hand:

```python
from llm_hops import parse_traceparent

with tracer.trace("request", parent=parse_traceparent(request.headers.get("traceparent"))) as root:
    ...
outgoing_headers["traceparent"] = root.traceparent
```

## What goes in a span

`service`, `name`, start and end in milliseconds, `status` (`ok` or `error`) and flat attributes: strings,
numbers, booleans. Attribute names the page understands: `client`, `model`, `role`, `wire`, `backend`,
`status_code`, `queue_ms`, `ttft_ms`, `prompt_tokens`, `completion_tokens`, `tok_per_s`, `error`, `rule`,
`cache`. Anything else is shown as it is. **Never put prompt or completion text in an attribute**; see the
project's `docs/privacy.md`.
