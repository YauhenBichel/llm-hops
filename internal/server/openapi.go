// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package server

import "net/http"

// openAPI describes the API for anyone generating a client: served at /api/openapi.json. Written by hand
// and kept short; the README's table says the same in prose.
const openAPI = `{
  "openapi": "3.1.0",
  "info": {"title": "llm-hops", "version": "1", "description": "Every hop of a request through a local LLM system. Times are milliseconds since the Unix epoch.",
           "license": {"name": "Apache-2.0"}},
  "servers": [{"url": "/"}],
  "components": {
    "securitySchemes": {"bearer": {"type": "http", "scheme": "bearer"}},
    "schemas": {
      "Span": {"type": "object", "required": ["trace_id", "start_ms"],
        "properties": {"trace_id": {"type": "string"}, "span_id": {"type": "string"}, "parent_id": {"type": ["string", "null"]},
          "service": {"type": "string"}, "name": {"type": "string"}, "start_ms": {"type": "integer"}, "end_ms": {"type": "integer"},
          "status": {"type": "string", "enum": ["ok", "error"]},
          "attrs": {"type": "object", "additionalProperties": {"type": ["string", "number", "boolean", "null"]},
                    "description": "flat; never prompt or completion text. Names the page understands: client, model, role, wire, backend, provider, status_code, queue_ms, ttft_ms, prompt_tokens, completion_tokens, tok_per_s, error, rule, cache"}}},
      "TraceSummary": {"type": "object", "properties": {"trace_id": {"type": "string"}, "start_ms": {"type": "integer"}, "end_ms": {"type": "integer"},
        "duration_ms": {"type": "integer"}, "service": {"type": "string"}, "name": {"type": "string"}, "status": {"type": "string"}, "spans": {"type": "integer"}, "attrs": {"type": "object"}}}
    }
  },
  "security": [{"bearer": []}, {}],
  "paths": {
    "/api/v1/spans": {"post": {"summary": "Send spans", "requestBody": {"required": true, "content": {"application/json": {"schema": {"oneOf": [
        {"type": "array", "items": {"$ref": "#/components/schemas/Span"}}, {"type": "object", "properties": {"spans": {"type": "array", "items": {"$ref": "#/components/schemas/Span"}}}}]}}}},
      "responses": {"200": {"description": "accepted, rejected, and the trace ids touched"}}}},
    "/v1/traces": {"post": {"summary": "OpenTelemetry OTLP/JSON traces (any OTel SDK's OTLP HTTP exporter, JSON encoding)",
      "responses": {"200": {"description": "accepted and skipped counts"}}}},
    "/api/v1/traces": {"get": {"summary": "Trace summaries, newest first", "parameters": [
        {"name": "since", "in": "query", "schema": {"type": "integer"}}, {"name": "until", "in": "query", "schema": {"type": "integer"}},
        {"name": "limit", "in": "query", "schema": {"type": "integer", "maximum": 5000}}, {"name": "model", "in": "query", "schema": {"type": "string"}},
        {"name": "client", "in": "query", "schema": {"type": "string"}}, {"name": "service", "in": "query", "schema": {"type": "string"}},
        {"name": "status", "in": "query", "schema": {"type": "string", "enum": ["ok", "error"]}}, {"name": "min_ms", "in": "query", "schema": {"type": "integer"}},
        {"name": "q", "in": "query", "schema": {"type": "string"}}],
      "responses": {"200": {"description": "traces", "content": {"application/json": {"schema": {"type": "object", "properties": {"traces": {"type": "array", "items": {"$ref": "#/components/schemas/TraceSummary"}}}}}}}}}},
    "/api/v1/traces/{id}": {"get": {"summary": "One trace: summary and every span", "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "responses": {"200": {"description": "trace and spans"}, "404": {"description": "no such trace"}}}},
    "/api/v1/stats": {"get": {"summary": "Counts, percentiles, per hop, per model, per backend (local or cloud), requests per bucket", "parameters": [
        {"name": "since", "in": "query", "schema": {"type": "integer"}}, {"name": "until", "in": "query", "schema": {"type": "integer"}}, {"name": "bucket_ms", "in": "query", "schema": {"type": "integer"}}],
      "responses": {"200": {"description": "stats"}}}},
    "/api/v1/flow": {"get": {"summary": "Services and the aggregated hops between them", "responses": {"200": {"description": "services and edges"}}}},
    "/api/v1/facets": {"get": {"summary": "Distinct values of model, client, wire, role, service in a window", "responses": {"200": {"description": "facets"}}}},
    "/api/v1/stream": {"get": {"summary": "Server-sent events: one per trace touched", "responses": {"200": {"description": "text/event-stream"}}}},
    "/api/v1/health": {"get": {"summary": "Liveness (never needs the token)", "security": [], "responses": {"200": {"description": "ok"}}}},
    "/metrics": {"get": {"summary": "Prometheus text: the last five minutes", "responses": {"200": {"description": "text/plain"}}}}
  }
}`

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(openAPI))
}
