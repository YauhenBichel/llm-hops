# Copyright 2026 Yauhen Bichel
# SPDX-License-Identifier: Apache-2.0
"""llm-hops SDK: open spans in your LLM service, post them to a llm-hops server."""

from .model import Span, TraceSummary, new_id, now_ms
from .sdk import ActiveSpan, Parent, Tracer, parse_traceparent, post_spans, traceparent

__version__ = "0.1.0"
__all__ = [
    "ActiveSpan",
    "Parent",
    "Span",
    "TraceSummary",
    "Tracer",
    "__version__",
    "new_id",
    "now_ms",
    "parse_traceparent",
    "post_spans",
    "traceparent",
]
