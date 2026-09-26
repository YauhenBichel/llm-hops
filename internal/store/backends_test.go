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
