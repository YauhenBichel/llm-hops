# Copyright 2026 Yauhen Bichel
# SPDX-License-Identifier: Apache-2.0
"""The span: the one record llm-hops stores.

A request through an LLM system is a trace: one root span (the request as the first service saw it) and
child spans for every hop it made (a routing decision, a wait in a queue, a model load, the model's own
prefill and generation, the reply). The shape follows OpenTelemetry's span closely enough that a converter is
a few lines, and stays small enough to write by hand in a log line.

Times are milliseconds since the Unix epoch, as integers. Attributes are a flat dict of strings, numbers and
booleans; prompt and completion text are never expected there (see docs/privacy.md).
"""

from __future__ import annotations

import secrets
import time
from dataclasses import dataclass, field
from typing import Any

AttrValue = str | int | float | bool | None
Attrs = dict[str, AttrValue]

# names the UI knows how to draw; anything else is drawn as a plain hop
KNOWN_HOPS = ("decide", "check", "queue", "load", "prefill", "generate", "upstream", "reply", "request")


def new_id(n: int = 8) -> str:
    return secrets.token_hex(n)


def now_ms() -> int:
    return int(time.time() * 1000)


@dataclass(slots=True)
class Span:
    trace_id: str
    span_id: str
    service: str
    name: str
    start_ms: int
    end_ms: int
    parent_id: str | None = None
    status: str = "ok"  # ok | error
    attrs: Attrs = field(default_factory=dict)

    @property
    def duration_ms(self) -> int:
        return max(0, self.end_ms - self.start_ms)

    def to_dict(self) -> dict[str, Any]:
        return {
            "trace_id": self.trace_id,
            "span_id": self.span_id,
            "parent_id": self.parent_id,
            "service": self.service,
            "name": self.name,
            "start_ms": self.start_ms,
            "end_ms": self.end_ms,
            "status": self.status,
            "attrs": self.attrs,
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Span:
        """Accept a span written by any client; reject what would corrupt the store, tolerate the rest."""
        try:
            trace_id = str(d["trace_id"])
            start_ms = int(d["start_ms"])
        except (KeyError, TypeError, ValueError) as e:
            raise ValueError(f"a span needs trace_id and start_ms: {e}") from e
        end_ms = int(d.get("end_ms") or start_ms)
        attrs_in = d.get("attrs") or {}
        if not isinstance(attrs_in, dict):
            raise ValueError("attrs must be an object")
        attrs: Attrs = {}
        for k, v in attrs_in.items():
            if isinstance(v, str | int | float | bool) or v is None:
                attrs[str(k)[:64]] = v[:512] if isinstance(v, str) else v
        status = str(d.get("status") or "ok")
        return cls(
            trace_id=trace_id[:64],
            span_id=str(d.get("span_id") or new_id())[:64],
            parent_id=(str(d["parent_id"])[:64] if d.get("parent_id") else None),
            service=str(d.get("service") or "unknown")[:64],
            name=str(d.get("name") or "request")[:64],
            start_ms=start_ms,
            end_ms=max(end_ms, start_ms),
            status="error" if status == "error" else "ok",
            attrs=attrs,
        )


@dataclass(slots=True)
class TraceSummary:
    """One row of the trace list: what the root span says, plus what the hops add up to."""

    trace_id: str
    start_ms: int
    end_ms: int
    service: str
    name: str
    status: str
    spans: int
    attrs: Attrs

    def to_dict(self) -> dict[str, Any]:
        return {
            "trace_id": self.trace_id,
            "start_ms": self.start_ms,
            "end_ms": self.end_ms,
            "duration_ms": max(0, self.end_ms - self.start_ms),
            "service": self.service,
            "name": self.name,
            "status": self.status,
            "spans": self.spans,
            "attrs": self.attrs,
        }
