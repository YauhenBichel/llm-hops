# Copyright 2026 Yauhen Bichel
# SPDX-License-Identifier: Apache-2.0
"""A pure ASGI middleware: one root span per HTTP request, the trace continued from an incoming
`traceparent` header, the `traceparent` returned so the caller can link its own trace, and the open span
placed in `scope["state"]["hops_span"]` for child spans inside the handler.

    app.add_middleware(HopsMiddleware, tracer=Tracer(url, service="gateway"))

    # in a handler
    span = request.state.hops_span
    with span.child("queue"):
        ...

Pure ASGI on purpose: a BaseHTTPMiddleware wraps the response in a way that breaks streaming and client
disconnect detection in many apps; this one only watches the messages go by. The span ends when the
response body ends, so a streamed reply is measured to its last byte.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable, MutableMapping
from typing import Any

from .sdk import ActiveSpan, Tracer, parse_traceparent

Scope = MutableMapping[str, Any]
Message = MutableMapping[str, Any]
Receive = Callable[[], Awaitable[Message]]
Send = Callable[[Message], Awaitable[None]]
ASGIApp = Callable[[Scope, Receive, Send], Awaitable[None]]

SKIP_PATHS = ("/healthz", "/metrics", "/api/v1/stream")


class HopsMiddleware:
    def __init__(
        self,
        app: ASGIApp,
        tracer: Tracer,
        *,
        name: str = "request",
        skip_paths: tuple[str, ...] = SKIP_PATHS,
        header: str = "traceparent",
    ) -> None:
        self.app = app
        self.tracer = tracer
        self.name = name
        self.skip_paths = skip_paths
        self.header = header.lower().encode()

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http" or scope.get("path", "") in self.skip_paths:
            await self.app(scope, receive, send)
            return
        headers = {k.lower(): v for k, v in scope.get("headers", [])}
        parent = parse_traceparent(headers.get(self.header, b"").decode("latin-1"))
        client = scope.get("client")
        attrs: dict[str, Any] = {"method": scope.get("method"), "path": scope.get("path")}
        if client:
            attrs["client"] = str(client[0])
        span = ActiveSpan(
            self.tracer,
            parent.trace_id if parent else _new_trace_id(),
            parent.span_id if parent else None,
            self.tracer.service,
            self.name,
            attrs,
        )
        scope.setdefault("state", {})["hops_span"] = span
        status_holder = {"code": 0}

        async def send_wrapped(message: Message) -> None:
            if message["type"] == "http.response.start":
                status_holder["code"] = int(message.get("status", 0))
                raw = list(message.get("headers", []))
                raw.append((b"traceparent", span.traceparent.encode()))
                message["headers"] = raw
            await send(message)
            if message["type"] == "http.response.body" and not message.get("more_body", False):
                _finish(span, status_holder["code"])

        try:
            await self.app(scope, receive, send_wrapped)
        except BaseException as e:
            span.attrs.setdefault("error", type(e).__name__)
            _finish(span, status_holder["code"] or 500, error=True)
            raise
        finally:
            _finish(span, status_holder["code"] or 499)


def _finish(span: ActiveSpan, status_code: int, error: bool = False) -> None:
    if span.end_ms is not None:
        return
    span.attrs["status_code"] = status_code
    span.end("error" if error or status_code >= 500 else "ok")


def _new_trace_id() -> str:
    from .model import new_id

    return new_id(16)
