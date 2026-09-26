// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"

	"github.com/YauhenBichel/llm-hops/internal/model"
)

func TestBackendsSplitLocalFromCloud(t *testing.T) {
	s := open(t)
	mk := func(id, backend, provider string, prompt, completion float64, status string) model.Span {
		return model.Span{TraceID: id, SpanID: "r", Service: "harness", Name: "request", StartMS: 1000, EndMS: 2000, Status: status,
			Attrs: model.Attrs{"backend": backend, "provider": provider, "model": "m", "prompt_tokens": prompt, "completion_tokens": completion}}
	}
	s.Add([]model.Span{
		mk("a", "local", "ollama", 100, 10, "ok"),
		mk("b", "local", "ollama", 200, 20, "error"),
		mk("c", "cloud", "anthropic", 1000, 100, "ok"),
		// a gateway line: its backend is a service name, which means local, and names the provider
		{TraceID: "d", SpanID: "r", Service: "gateway", Name: "request", StartMS: 1000, EndMS: 1500, Status: "ok",
			Attrs: model.Attrs{"backend": "cpu", "model": "bge-m3", "prompt_tokens": 5.0}},
	})
	st, err := s.Stats(0, 10_000, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Backends) != 3 {
		t.Fatalf("backends: %+v", st.Backends)
	}
	if st.Backends[0].Backend != "cloud" || st.Backends[0].Provider != "anthropic" || st.Backends[0].PromptTokens != 1000 || st.Backends[0].CompletionTokens != 100 {
		t.Fatalf("cloud first: %+v", st.Backends[0])
	}
	var local, cpu *BackendStat
	for i := range st.Backends {
		switch st.Backends[i].Provider {
		case "ollama":
			local = &st.Backends[i]
		case "cpu":
			cpu = &st.Backends[i]
		}
	}
	if local == nil || local.Backend != "local" || local.Requests != 2 || local.Errors != 1 || local.PromptTokens != 300 || local.P50MS != 1000 {
		t.Fatalf("local: %+v", local)
	}
	if cpu == nil || cpu.Backend != "local" || cpu.Requests != 1 || cpu.PromptTokens != 5 {
		t.Fatalf("cpu: %+v", cpu)
	}
}

func TestLoadsAreCountedAndNotRequests(t *testing.T) {
	s := open(t)
	s.Add([]model.Span{
		{TraceID: "r1", SpanID: "root", Service: "gateway", Name: "request", StartMS: 1000, EndMS: 2000, Status: "ok", Attrs: model.Attrs{"model": "m"}},
		{TraceID: "l1", SpanID: "l1", Service: "ollama", Name: "load", StartMS: 1000, EndMS: 31000, Status: "ok", Attrs: model.Attrs{"model": "m", "kind": "load", "load_ms": 30000.0}},
		{TraceID: "l2", SpanID: "l2", Service: "ollama", Name: "load", StartMS: 40000, EndMS: 50000, Status: "ok", Attrs: model.Attrs{"model": "n", "kind": "load"}},
	})
	st, _ := s.Stats(0, 100000, 60000)
	if st.Requests != 1 || st.Loads != 2 || st.LoadMS != 40000 || st.P50MS != 1000 {
		t.Fatalf("%+v", st)
	}
}

func TestLoadsIncludeTheOneBeforeTheWindow(t *testing.T) {
	s := open(t)
	mk := func(id, m string, a, b int64) model.Span {
		return model.Span{TraceID: id, SpanID: id, Service: "ollama", Name: "load", StartMS: a, EndMS: b, Status: "ok", Attrs: model.Attrs{"model": m, "kind": "load"}}
	}
	s.Add([]model.Span{mk("a", "x", 100, 200), mk("b", "y", 1000, 1100), mk("c", "x", 5000, 5100), mk("d", "z", 9000, 9100)})
	loads, err := s.Loads(2000, 8000)
	if err != nil || len(loads) != 2 || loads[0].Model != "y" || loads[1].Model != "x" {
		t.Fatalf("%+v %v", loads, err)
	}
	loads, _ = s.Loads(50, 8000)
	if len(loads) != 3 {
		t.Fatalf("%+v", loads)
	}
}

func TestTracesListLeavesLoadsOutUnlessAsked(t *testing.T) {
	s := open(t)
	s.Add([]model.Span{
		{TraceID: "r", SpanID: "r", Service: "gateway", Name: "request", StartMS: 1, EndMS: 2, Status: "ok", Attrs: model.Attrs{"model": "m"}},
		{TraceID: "l", SpanID: "l", Service: "ollama", Name: "load", StartMS: 1, EndMS: 2, Status: "ok", Attrs: model.Attrs{"model": "m", "kind": "load"}},
	})
	if rows, _ := s.Traces(Filter{}); len(rows) != 1 || rows[0].TraceID != "r" {
		t.Fatalf("default: %+v", rows)
	}
	if rows, _ := s.Traces(Filter{Kind: "load"}); len(rows) != 1 || rows[0].TraceID != "l" {
		t.Fatalf("load: %+v", rows)
	}
	if rows, _ := s.Traces(Filter{Kind: "any"}); len(rows) != 2 {
		t.Fatalf("any: %+v", rows)
	}
}
