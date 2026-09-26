# Copyright 2026 Yauhen Bichel
# SPDX-License-Identifier: Apache-2.0
"""The SDK without a server: spans are shaped right, a trace continues from a traceparent, the middleware
makes one span per request and ends it with the body, and a server that is down costs nothing."""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import pytest
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import PlainTextResponse, StreamingResponse
from starlette.routing import Route
from starlette.testclient import TestClient

from llm_hops import Span, Tracer, parse_traceparent, traceparent
from llm_hops.asgi import HopsMiddleware


def test_span_from_dict_tolerates_and_rejects() -> None:
    s = Span.from_dict(
        {"trace_id": "t", "start_ms": 5, "attrs": {"a": "x", "n": 1, "nested": {"no": 1}, "long": "y" * 600}}
    )
    assert s.span_id and s.service == "unknown" and s.name == "request" and s.end_ms == 5 and s.status == "ok"
    assert "nested" not in s.attrs and len(str(s.attrs["long"])) == 512
    with pytest.raises(ValueError):
        Span.from_dict({"trace_id": "t"})
    with pytest.raises(ValueError):
        Span.from_dict({"trace_id": "t", "start_ms": 1, "attrs": "nope"})
    assert Span.from_dict({"trace_id": "t", "start_ms": 10, "end_ms": 4, "status": "odd"}).end_ms == 10


def test_tracer_in_memory_nests_and_marks_errors() -> None:
    tr = Tracer(None, service="gateway")
    with tr.trace("request", attrs={"client": "editor"}) as root:
        with root.child("queue"):
            pass
        with pytest.raises(RuntimeError), root.child("upstream", service="ollama"):
            raise RuntimeError("boom")
    spans = tr.drain()
    assert [s.name for s in spans] == ["queue", "upstream", "request"]
    assert all(s.trace_id == root.trace_id for s in spans)
    assert spans[0].parent_id == root.span_id and spans[2].parent_id is None
    up = spans[1]
    assert up.status == "error" and up.attrs["error"] == "RuntimeError" and up.service == "ollama"
    assert spans[2].attrs == {"client": "editor"} and spans[2].end_ms >= spans[2].start_ms


def test_traceparent_round_trip_and_continuation() -> None:
    tp = traceparent("abc", "def")
    p = parse_traceparent(tp)
    assert p is not None and p.trace_id.endswith("abc") and p.span_id.endswith("def") and len(p.trace_id) == 32
    assert parse_traceparent("garbage") is None and parse_traceparent(None) is None
    tr = Tracer(None, service="router")
    with tr.trace("request", parent=p) as s:
        assert s.trace_id == p.trace_id and s.parent_id == p.span_id


class _Sink(BaseHTTPRequestHandler):
    received: list[dict[str, Any]] = []

    def do_POST(self) -> None:  # noqa: N802
        n = int(self.headers.get("content-length", 0))
        body = json.loads(self.rfile.read(n))
        _Sink.received.extend(body)
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"accepted": len(body), "rejected": 0, "traces": []}).encode())

    def log_message(self, *a: Any) -> None:
        pass


@pytest.fixture
def sink() -> Any:
    srv = HTTPServer(("127.0.0.1", 0), _Sink)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    _Sink.received = []
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


def test_tracer_posts_in_batches_from_a_thread(sink: str) -> None:
    tr = Tracer(sink, service="app", batch=3, flush_s=0.2)
    for i in range(5):
        with tr.trace("request", attrs={"i": i}):
            pass
    tr.close()
    assert len(_Sink.received) == 5 and tr.sent == 5 and tr.failed_posts == 0
    assert {r["attrs"]["i"] for r in _Sink.received} == {0, 1, 2, 3, 4}


def test_a_dead_server_costs_nothing() -> None:
    tr = Tracer("http://127.0.0.1:9", service="app", flush_s=0.05, timeout_s=0.3)
    t0 = time.monotonic()
    for _ in range(50):
        with tr.trace("request"):
            pass
    assert time.monotonic() - t0 < 0.5  # never blocks the caller
    tr.close()
    assert tr.failed_posts >= 1 and tr.sent == 0


def test_middleware_one_span_per_request_ended_with_the_body() -> None:
    tr = Tracer(None, service="gateway")

    async def hello(request: Request) -> PlainTextResponse:
        span = request.state.hops_span
        with span.child("queue"):
            pass
        return PlainTextResponse("hi")

    async def stream(request: Request) -> StreamingResponse:
        async def gen() -> Any:
            yield b"a"
            yield b"b"

        return StreamingResponse(gen())

    async def boom(request: Request) -> PlainTextResponse:
        raise ValueError("no")

    app = Starlette(
        routes=[Route("/hello", hello), Route("/stream", stream), Route("/boom", boom), Route("/healthz", hello)]
    )
    app.add_middleware(HopsMiddleware, tracer=tr)
    c = TestClient(app, raise_server_exceptions=False)

    r = c.get("/hello", headers={"traceparent": traceparent("f" * 32, "1" * 16)})
    assert r.status_code == 200 and r.headers["traceparent"].startswith("00-" + "f" * 32)
    spans = tr.drain()
    assert [s.name for s in spans] == ["queue", "request"]
    root = spans[1]
    assert root.trace_id == "f" * 32 and root.parent_id == "1" * 16 and root.attrs["status_code"] == 200
    assert root.attrs["path"] == "/hello" and root.attrs["method"] == "GET" and root.status == "ok"

    assert c.get("/stream").text == "ab"
    (s,) = tr.drain()
    assert s.attrs["status_code"] == 200 and s.end_ms >= s.start_ms

    assert c.get("/boom").status_code == 500
    (s,) = tr.drain()
    assert s.status == "error" and s.attrs["error"] == "ValueError"

    c.get("/healthz")
    assert tr.drain() == []  # skipped paths make no span
