# Copyright 2026 Yauhen Bichel
# SPDX-License-Identifier: Apache-2.0
"""The client side, standard library only: open spans in your code, they are posted in batches from a
background thread, and a server that is down costs you nothing but a warning.

    tracer = Tracer("http://127.0.0.1:11602", service="gateway")
    with tracer.trace("request", attrs={"client": ip, "model": model}) as root:
        with root.child("queue"):
            ...
        with root.child("upstream", attrs={"model": model}) as up:
            ...
            up.attrs["ttft_ms"] = ttft
    tracer.close()   # flushes

Trace ids travel between services in the W3C `traceparent` header: `traceparent()` makes one from a span,
`parse_traceparent()` reads one, and `tracer.trace(..., parent=parse_traceparent(h))` continues the trace.
"""

from __future__ import annotations

import json
import logging
import queue
import re
import threading
import time
import urllib.error
import urllib.request
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Any

from .model import Attrs, Span, new_id, now_ms

log = logging.getLogger("llm_hops")


class _Tick:
    """What the queue hands back when nothing arrived within flush_s."""


_TICK = _Tick()
_TP = re.compile(r"^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$")


@dataclass(slots=True)
class Parent:
    trace_id: str
    span_id: str


def traceparent(trace_id: str, span_id: str) -> str:
    """A W3C traceparent for the ids llm-hops uses (32 and 16 hex characters; shorter ids are padded)."""
    return f"00-{trace_id[:32].rjust(32, '0')}-{span_id[:16].rjust(16, '0')}-01"


def parse_traceparent(value: str | None) -> Parent | None:
    if not value:
        return None
    m = _TP.match(value.strip())
    return Parent(m.group(1), m.group(2)) if m else None


class ActiveSpan:
    """A span that is open. Set `attrs` and `status` while it runs; it is sent when it ends."""

    __slots__ = (
        "_tracer",
        "attrs",
        "end_ms",
        "name",
        "parent_id",
        "service",
        "span_id",
        "start_ms",
        "status",
        "trace_id",
    )

    def __init__(
        self, tracer: Tracer, trace_id: str, parent_id: str | None, service: str, name: str, attrs: Attrs | None
    ) -> None:
        self._tracer = tracer
        self.trace_id = trace_id
        self.span_id = new_id()
        self.parent_id = parent_id
        self.service = service
        self.name = name
        self.start_ms = now_ms()
        self.end_ms: int | None = None
        self.status = "ok"
        self.attrs: Attrs = dict(attrs or {})

    @contextmanager
    def child(self, name: str, attrs: Attrs | None = None, service: str | None = None) -> Iterator[ActiveSpan]:
        with self._tracer.span(name, trace_id=self.trace_id, parent_id=self.span_id, attrs=attrs, service=service) as s:
            yield s

    def end(self, status: str | None = None) -> None:
        if self.end_ms is not None:
            return
        if status:
            self.status = status
        self.end_ms = now_ms()
        self._tracer.emit(
            Span(
                self.trace_id,
                self.span_id,
                self.service,
                self.name,
                self.start_ms,
                self.end_ms,
                self.parent_id,
                self.status,
                self.attrs,
            )
        )

    @property
    def traceparent(self) -> str:
        return traceparent(self.trace_id, self.span_id)


class Tracer:
    def __init__(
        self,
        url: str | None = "http://127.0.0.1:11602",
        *,
        service: str = "app",
        batch: int = 100,
        flush_s: float = 1.0,
        queue_size: int = 10_000,
        timeout_s: float = 5.0,
    ) -> None:
        """`url` None keeps spans in memory (`tracer.drain()` returns them): for tests, or for writing them
        to your own log."""
        self.url = url.rstrip("/") + "/api/v1/spans" if url else None
        self.service = service
        self.batch = batch
        self.flush_s = flush_s
        self.timeout_s = timeout_s
        self._q: queue.Queue[Span | _Tick | None] = queue.Queue(maxsize=queue_size)
        self._kept: list[Span] = []
        self.dropped = 0
        self.sent = 0
        self.failed_posts = 0
        self._thread: threading.Thread | None = None
        if self.url:
            self._thread = threading.Thread(target=self._run, name="llm-hops-tracer", daemon=True)
            self._thread.start()

    # ---- opening spans ------------------------------------------------------------------------------

    @contextmanager
    def trace(
        self,
        name: str = "request",
        attrs: Attrs | None = None,
        parent: Parent | None = None,
        service: str | None = None,
    ) -> Iterator[ActiveSpan]:
        """A root span: a new trace, or the continuation of one from a `traceparent` header."""
        with self.span(
            name,
            trace_id=parent.trace_id if parent else new_id(16),
            parent_id=parent.span_id if parent else None,
            attrs=attrs,
            service=service,
        ) as s:
            yield s

    @contextmanager
    def span(
        self, name: str, *, trace_id: str, parent_id: str | None, attrs: Attrs | None = None, service: str | None = None
    ) -> Iterator[ActiveSpan]:
        s = ActiveSpan(self, trace_id, parent_id, service or self.service, name, attrs)
        try:
            yield s
        except BaseException as e:
            s.attrs.setdefault("error", type(e).__name__)
            s.end("error")
            raise
        else:
            s.end()

    # ---- sending ---------------------------------------------------------------------------------------

    def emit(self, span: Span) -> None:
        if self.url is None:
            self._kept.append(span)
            return
        try:
            self._q.put_nowait(span)
        except queue.Full:
            self.dropped += 1

    def drain(self) -> list[Span]:
        out, self._kept = self._kept, []
        return out

    def flush(self, timeout_s: float = 5.0) -> None:
        """Wait until everything queued so far has been posted (or given up on)."""
        deadline = time.monotonic() + timeout_s
        while not self._q.empty() and time.monotonic() < deadline:
            time.sleep(0.02)
        time.sleep(0.05)

    def close(self) -> None:
        if self._thread:
            self._q.put(None)
            self._thread.join(timeout=self.timeout_s + 1)

    def _run(self) -> None:
        pending: list[Span] = []
        last = time.monotonic()
        while True:
            try:
                item = self._q.get(timeout=self.flush_s)
            except queue.Empty:
                item = _TICK
            if item is None:
                self._post(pending)
                return
            if isinstance(item, Span):
                pending.append(item)
            if len(pending) >= self.batch or (pending and time.monotonic() - last >= self.flush_s):
                self._post(pending)
                pending = []
                last = time.monotonic()

    def _post(self, spans: list[Span]) -> None:
        if not spans or not self.url:
            return
        body = json.dumps([s.to_dict() for s in spans], separators=(",", ":")).encode()
        req = urllib.request.Request(self.url, data=body, headers={"content-type": "application/json"}, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout_s) as r:  # noqa: S310 (a URL the caller set)
                r.read()
            self.sent += len(spans)
        except (urllib.error.URLError, OSError, ValueError) as e:
            self.failed_posts += 1
            if self.failed_posts in (1, 10, 100) or self.failed_posts % 1000 == 0:
                log.warning(
                    "llm-hops: could not post %d spans to %s (%s); failures so far: %d",
                    len(spans),
                    self.url,
                    e,
                    self.failed_posts,
                )


def post_spans(url: str, spans: list[Span], timeout_s: float = 5.0) -> dict[str, Any]:
    """One synchronous post, for adapters and scripts."""
    body = json.dumps([s.to_dict() for s in spans], separators=(",", ":")).encode()
    req = urllib.request.Request(
        url.rstrip("/") + "/api/v1/spans", data=body, headers={"content-type": "application/json"}, method="POST"
    )
    with urllib.request.urlopen(req, timeout=timeout_s) as r:  # noqa: S310
        out: dict[str, Any] = json.loads(r.read())
        return out
