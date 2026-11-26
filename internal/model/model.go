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
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// What a sender must respect; Validate enforces it for every way in (the API, OTLP, an import).
const (
	// MaxIDLen is the longest trace_id, span_id or parent_id, in bytes. A longer one is rejected, never
	// shortened: a shortened id could not be looked up again, and two ids with the same first 64 bytes
	// would merge into one trace.
	MaxIDLen = 64
	// MaxSpanMS is the longest span: a longer one is a unit mix-up (seconds or nanoseconds sent as
	// milliseconds) or a skewed clock, and would become the window's maximum.
	MaxSpanMS = 24 * 3_600_000
	// MaxAheadMS is how far in the future a span may start, for a sender whose clock runs ahead.
	MaxAheadMS = 24 * 3_600_000
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
// Ids are strings or numbers of at most MaxIDLen bytes without control characters; start_ms is not negative
// and not more than MaxAheadMS in the future; a span lasts at most MaxSpanMS. Service, name and attributes
// are cut to size instead.
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
		return Span{}, errors.New("a span is a JSON object")
	}
	var ids [3]string
	for i, f := range []struct {
		name string
		v    any
	}{{"trace_id", in.TraceID}, {"span_id", in.SpanID}, {"parent_id", in.ParentID}} {
		switch f.v.(type) {
		case nil, string, float64:
			ids[i] = str(f.v)
		default:
			return Span{}, fmt.Errorf("%s must be a string", f.name)
		}
	}
	start, ok := num(in.StartMS)
	if ids[0] == "" || !ok {
		return Span{}, errors.New("a span needs trace_id and start_ms")
	}
	end, ok := num(in.EndMS)
	if !ok || end < start {
		end = start
	}
	// checked as floats: converting 1e30 to an integer would wrap to an arbitrary time
	if start < 0 || start > math.MaxInt64/2 || end > math.MaxInt64/2 {
		return Span{}, errors.New("start_ms and end_ms are milliseconds since 1970, not negative")
	}
	s := Span{
		TraceID:  ids[0],
		SpanID:   ids[1],
		ParentID: ids[2],
		Service:  cut(printable(str(in.Service)), 64),
		Name:     cut(printable(str(in.Name)), 64),
		StartMS:  int64(start),
		EndMS:    int64(end),
		Status:   "ok",
		Attrs:    Attrs{},
	}
	if s.SpanID == "" {
		s.SpanID = NewID(8)
	}
	if err := Validate(s, NowMS()); err != nil {
		return Span{}, err
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

// Validate says why a span cannot be stored, or nil: the limits in this package's constants. The message
// goes back to the sender.
func Validate(s Span, nowMS int64) error {
	if s.TraceID == "" {
		return errors.New("a span needs trace_id and start_ms")
	}
	for _, f := range []struct{ name, v string }{{"trace_id", s.TraceID}, {"span_id", s.SpanID}, {"parent_id", s.ParentID}} {
		if len(f.v) > MaxIDLen {
			return fmt.Errorf("%s is longer than %d bytes (ids are stored whole, never shortened)", f.name, MaxIDLen)
		}
		if !utf8.ValidString(f.v) || strings.ContainsFunc(f.v, isControl) {
			return fmt.Errorf("%s has a control character or is not UTF-8", f.name)
		}
	}
	switch {
	case s.StartMS < 0:
		return errors.New("start_ms and end_ms are milliseconds since 1970, not negative")
	case s.StartMS > nowMS+MaxAheadMS:
		return errors.New("start_ms is more than a day in the future (milliseconds since 1970 are expected)")
	case s.EndMS-s.StartMS > MaxSpanMS:
		return errors.New("the span is longer than 24 hours (milliseconds are expected for start_ms and end_ms)")
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// printable drops control characters, which a name or a service never needs.
func printable(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
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
		if _, err := fmt.Sscan(t, &f); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, true
		}
	}
	return 0, false
}

// cut shortens s to at most n bytes, never in the middle of a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
