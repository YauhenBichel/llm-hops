// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"strings"
	"testing"
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
