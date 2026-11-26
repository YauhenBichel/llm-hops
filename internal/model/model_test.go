// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCleanFillsWhatIsMissingAndCutsWhatIsLong(t *testing.T) {
	s, err := Clean(json.RawMessage(`{"trace_id":"t","start_ms":100,"attrs":{"a":"x","n":2,"b":true,"nested":{"no":1},"long":"` + strings.Repeat("y", 600) + `"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.SpanID == "" || s.Service != "unknown" || s.Name != "request" || s.EndMS != 100 || s.Status != "ok" {
		t.Fatalf("%+v", s)
	}
	if _, ok := s.Attrs["nested"]; ok {
		t.Fatal("a nested attribute was kept")
	}
	if len(s.Attrs["long"].(string)) != 512 {
		t.Fatal("a long attribute was not cut")
	}
	if s.Attrs["n"] != 2.0 || s.Attrs["b"] != true {
		t.Fatalf("%v", s.Attrs)
	}
}

func TestCleanRejectsWhatWouldCorrupt(t *testing.T) {
	for _, raw := range []string{`{}`, `{"trace_id":"t"}`, `{"start_ms":1}`, `{"trace_id":"","start_ms":1}`, `[]`, `"x"`} {
		if _, err := Clean(json.RawMessage(raw)); err == nil {
			t.Fatalf("%s was accepted", raw)
		}
	}
	s, _ := Clean(json.RawMessage(`{"trace_id":"t","start_ms":10,"end_ms":5,"status":"weird"}`))
	if s.EndMS != 10 || s.Status != "ok" {
		t.Fatalf("an end before the start or an unknown status must be normalised: %+v", s)
	}
}

func TestIDs(t *testing.T) {
	if len(NewID(8)) != 16 || NewID(8) == NewID(8) {
		t.Fatal("ids")
	}
}

func TestCleanRejectsAnIDItWouldHaveToShorten(t *testing.T) {
	long := strings.Repeat("X", 64)
	if s, err := Clean(json.RawMessage(`{"trace_id":"` + long + `","span_id":"` + long + `","parent_id":"` + long + `","start_ms":1}`)); err != nil || s.TraceID != long {
		t.Fatalf("64 bytes is the limit, not over it: %v", err)
	}
	// before: both were cut to the same 64 bytes and merged into one trace, and neither could be looked up
	for _, field := range []string{"trace_id", "span_id", "parent_id"} {
		raw := map[string]any{"trace_id": "t", "span_id": "s", "start_ms": 1}
		raw[field] = long + "A"
		body, _ := json.Marshal(raw)
		_, err := Clean(body)
		if err == nil || !strings.Contains(err.Error(), field+" is longer than 64 bytes") {
			t.Fatalf("%s of 65 bytes: %v", field, err)
		}
	}
}

func TestCleanRejectsTimesAndIDsThatCannotBeRight(t *testing.T) {
	now := NowMS()
	for name, raw := range map[string]string{
		"a start of 1e30":          `{"trace_id":"t","start_ms":1e30}`,
		"a negative start":         `{"trace_id":"t","start_ms":-5,"end_ms":3}`,
		"a start next year":        `{"trace_id":"t","start_ms":` + fmt.Sprint(now+400*86_400_000) + `}`,
		"a span of thirty years":   `{"trace_id":"t","start_ms":` + fmt.Sprint(now-1_000_000_000) + `,"end_ms":` + fmt.Sprint(now+1_000_000_000_000) + `}`,
		"an end of 1e30":           `{"trace_id":"t","start_ms":1,"end_ms":1e30}`,
		"NaN as text":              `{"trace_id":"t","start_ms":"NaN"}`,
		"a NUL in the trace id":    `{"trace_id":"u\u0000x","start_ms":1}`,
		"a newline in the span id": `{"trace_id":"t","span_id":"a\nb","start_ms":1}`,
		"an object as trace id":    `{"trace_id":{"a":1},"start_ms":1}`,
		"a list as span id":        `{"trace_id":"t","span_id":[1],"start_ms":1}`,
		"true as trace id":         `{"trace_id":true,"start_ms":1}`,
	} {
		if s, err := Clean(json.RawMessage(raw)); err == nil {
			t.Fatalf("%s was accepted: %+v", name, s)
		}
	}
	// what stays fine: a number as an id, a clock a little ahead, a long but possible span, old data
	for name, raw := range map[string]string{
		"a number as id":    `{"trace_id":42,"span_id":7,"start_ms":1}`,
		"an hour ahead":     `{"trace_id":"t","start_ms":` + fmt.Sprint(now+3_600_000) + `}`,
		"a span of an hour": `{"trace_id":"t","start_ms":1000,"end_ms":3601000}`,
		"a start in 1970":   `{"trace_id":"t","start_ms":0}`,
		"times as text":     `{"trace_id":"t","start_ms":"1000","end_ms":"2000"}`,
	} {
		if _, err := Clean(json.RawMessage(raw)); err != nil {
			t.Fatalf("%s was rejected: %v", name, err)
		}
	}
	s, err := Clean(json.RawMessage(`{"trace_id":"t","start_ms":1,"service":"g\u0000w","name":"re\u0007quest"}`))
	if err != nil || s.Service != "gw" || s.Name != "request" {
		t.Fatalf("control characters in a service or a name are dropped: %+v %v", s, err)
	}
}

func TestCutKeepsCharactersWhole(t *testing.T) {
	s, err := Clean(json.RawMessage(`{"trace_id":"t","start_ms":1,"attrs":{"a":"` + strings.Repeat("é", 300) + `"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if v := s.Attrs["a"].(string); len(v) != 512 || !utf8.ValidString(v) {
		t.Fatalf("%d bytes, valid UTF-8: %v", len(v), utf8.ValidString(v))
	}
	if got := cut("ééé", 3); got != "é" {
		t.Fatalf("cut in the middle of a character: %q", got)
	}
}
