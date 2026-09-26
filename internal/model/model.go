// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package model holds the span: the one record llm-hops stores.
//
// A request through an LLM system is a trace: one root span (the request as the first service saw it)
// and child spans for every hop it made (a routing decision, a wait in a queue, a model load, the model's
// own prefill and generation, the reply). The shape follows OpenTelemetry's span closely enough that a
// converter is a few lines, and stays small enough to write by hand in a log line.
//
// Times are milliseconds since the Unix epoch. Attributes are a flat map of strings, numbers and booleans;
// prompt and completion text are never expected there.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Attrs is a flat map: strings, numbers, booleans or null.
type Attrs map[string]any

// Span is one hop of one request.
type Span struct {
	TraceID  string `json:"trace_id"`
	SpanID   string `json:"span_id"`
	ParentID string `json:"parent_id,omitempty"`
	Service  string `json:"service"`
	Name     string `json:"name"`
	StartMS  int64  `json:"start_ms"`
	EndMS    int64  `json:"end_ms"`
	Status   string `json:"status"` // ok | error
	Attrs    Attrs  `json:"attrs"`
}

// TraceSummary is one row of the trace list: what the root span says, plus what the hops add up to.
type TraceSummary struct {
	TraceID    string `json:"trace_id"`
	StartMS    int64  `json:"start_ms"`
	EndMS      int64  `json:"end_ms"`
	DurationMS int64  `json:"duration_ms"`
	Service    string `json:"service"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Spans      int    `json:"spans"`
	Attrs      Attrs  `json:"attrs"`
}

// SummaryAttrs are the root span's attributes the list view filters and shows; the rest stay on the span.
var SummaryAttrs = []string{"client", "wire", "role", "model", "requested_model", "backend", "provider", "stream", "status_code",
	"prompt_tokens", "completion_tokens", "tok_per_s", "error", "rule", "locked", "queue_ms", "ttft_ms", "prompt_chars",
	"kind", "load_ms", "blob"}

// NewID returns n random bytes as hex.
func NewID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// NowMS is the current time in milliseconds since the epoch.
func NowMS() int64 { return time.Now().UnixMilli() }

// Duration is the span's length, never negative.
func (s Span) Duration() int64 {
	if s.EndMS < s.StartMS {
		return 0
	}
	return s.EndMS - s.StartMS
}

// Clean accepts a span written by any client: it rejects what would corrupt the store and tolerates the rest.
func Clean(raw json.RawMessage) (Span, error) {
	var in struct {
		TraceID  any            `json:"trace_id"`
		SpanID   any            `json:"span_id"`
		ParentID any            `json:"parent_id"`
		Service  any            `json:"service"`
		Name     any            `json:"name"`
		StartMS  any            `json:"start_ms"`
		EndMS    any            `json:"end_ms"`
		Status   any            `json:"status"`
		Attrs    map[string]any `json:"attrs"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Span{}, err
	}
	tid := str(in.TraceID)
	start, ok := num(in.StartMS)
	if tid == "" || !ok {
		return Span{}, errors.New("a span needs trace_id and start_ms")
	}
	end, ok := num(in.EndMS)
	if !ok || end < start {
		end = start
	}
	s := Span{
		TraceID:  cut(tid, 64),
		SpanID:   cut(str(in.SpanID), 64),
		ParentID: cut(str(in.ParentID), 64),
		Service:  cut(str(in.Service), 64),
		Name:     cut(str(in.Name), 64),
		StartMS:  int64(start),
		EndMS:    int64(end),
		Status:   "ok",
		Attrs:    Attrs{},
	}
	if s.SpanID == "" {
		s.SpanID = NewID(8)
	}
	if s.Service == "" {
		s.Service = "unknown"
	}
	if s.Name == "" {
		s.Name = "request"
	}
	if str(in.Status) == "error" {
		s.Status = "error"
	}
	for k, v := range in.Attrs {
		switch t := v.(type) {
		case string:
			s.Attrs[cut(k, 64)] = cut(t, 512)
		case float64, bool, nil:
			s.Attrs[cut(k, 64)] = t
		}
	}
	return s, nil
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%v", t)
	case nil:
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		var f float64
		if _, err := fmt.Sscan(t, &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
