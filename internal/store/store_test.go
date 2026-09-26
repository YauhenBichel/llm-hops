// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"testing"

	"github.com/YauhenBichel/llm-hops/internal/demo"
	"github.com/YauhenBichel/llm-hops/internal/model"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAddBuildsASummaryFromTheRootAndTheHops(t *testing.T) {
	s := open(t)
	root := model.Span{TraceID: "t1", SpanID: "r", Service: "gateway", Name: "request", StartMS: 1000, EndMS: 1000, Status: "ok",
		Attrs: model.Attrs{"client": "editor", "wire": "openai", "prompt_tokens": 12.0, "secret": "never listed"}}
	child := model.Span{TraceID: "t1", SpanID: "c", ParentID: "r", Service: "ollama", Name: "upstream", StartMS: 1010, EndMS: 1500, Status: "error",
		Attrs: model.Attrs{"model": "qwen"}}
	touched, err := s.Add([]model.Span{child, root})
	if err != nil || len(touched) != 1 || touched[0] != "t1" {
		t.Fatalf("touched=%v err=%v", touched, err)
	}
	sum, _ := s.Summary("t1")
	if sum == nil || sum.StartMS != 1000 || sum.EndMS != 1500 || sum.Status != "error" || sum.Spans != 2 || sum.Service != "gateway" {
		t.Fatalf("summary: %+v", sum)
	}
	if sum.Attrs["model"] != "qwen" || sum.Attrs["client"] != "editor" {
		t.Fatalf("the child's model and the root's client belong in the summary: %v", sum.Attrs)
	}
	if _, ok := sum.Attrs["secret"]; ok {
		t.Fatal("an attribute outside the summary list leaked into the summary")
	}
	// a repeat of the same span replaces it
	root.EndMS = 1600
	s.Add([]model.Span{root})
	sum, _ = s.Summary("t1")
	if sum.EndMS != 1600 || sum.Spans != 2 {
		t.Fatalf("after replace: %+v", sum)
	}
}

func TestTracesFilterAndOrder(t *testing.T) {
	s := open(t)
	g := demo.New(1, 1_000_000, 60, 600)
	for i := 0; i < 60; i++ {
		if _, err := s.Add(g.Next()); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.Traces(Filter{})
	if len(all) != 60 {
		t.Fatalf("got %d traces", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].StartMS > all[i-1].StartMS {
			t.Fatal("not newest first")
		}
	}
	errs, _ := s.Traces(Filter{Status: "error"})
	for _, tr := range errs {
		if tr.Status != "error" {
			t.Fatal("status filter")
		}
	}
	coder, _ := s.Traces(Filter{Model: "qwen3-coder-next"})
	if len(coder) == 0 {
		t.Fatal("no coder traces in 60 synthetic ones")
	}
	for _, tr := range coder {
		if tr.Attrs["model"] != "qwen3-coder-next" {
			t.Fatal("model filter")
		}
	}
	since := all[10].StartMS
	recent, _ := s.Traces(Filter{Since: &since, Limit: 5})
	if len(recent) != 5 {
		t.Fatalf("limit: %d", len(recent))
	}
	q, _ := s.Traces(Filter{Q: "editor"})
	for _, tr := range q {
		if tr.Attrs["client"] != "editor" {
			t.Fatal("search")
		}
	}
}

func TestStatsFlowFacetsAndPrune(t *testing.T) {
	s := open(t)
	g := demo.New(3, 1_000_000, 80, 800)
	for i := 0; i < 80; i++ {
		s.Add(g.Next())
	}
	st, err := s.Stats(0, 10_000_000, 60_000)
	if err != nil || st.Requests != 80 {
		t.Fatalf("stats: %+v %v", st, err)
	}
	if st.P50MS <= 0 || st.P95MS < st.P50MS || st.MaxMS < st.P95MS {
		t.Fatalf("percentiles: %+v", st)
	}
	if _, ok := st.Hops["prefill"]; !ok {
		t.Fatalf("hops: %v", st.Hops)
	}
	if len(st.Models) == 0 || st.Models[0].Requests < st.Models[len(st.Models)-1].Requests {
		t.Fatalf("models: %+v", st.Models)
	}
	if len(st.Buckets) == 0 {
		t.Fatal("buckets")
	}
	edges, _ := s.Edges(0, 10_000_000)
	var sawGatewayToOllama bool
	for _, e := range edges {
		if e.From == "gateway:request" && e.To == "ollama:upstream" && e.N > 0 {
			sawGatewayToOllama = true
		}
	}
	if !sawGatewayToOllama {
		t.Fatalf("edges: %+v", edges)
	}
	svcs, _ := s.Services(0, 10_000_000)
	if len(svcs) < 3 {
		t.Fatalf("services: %+v", svcs)
	}
	f, _ := s.Facets(0, 10_000_000)
	if len(f["model"]) < 2 || len(f["client"]) < 2 || len(f["service"]) < 1 {
		t.Fatalf("facets: %v", f)
	}
	n, err := s.Prune(1, 20_000_000)
	if err != nil || n != 80 {
		t.Fatalf("prune: %d %v", n, err)
	}
	left, _ := s.Traces(Filter{})
	if len(left) != 0 {
		t.Fatal("prune left traces")
	}
	spans, _ := s.Trace(st.Models[0].Model)
	if len(spans) != 0 {
		t.Fatal("prune left spans")
	}
}

func TestOpenSetsWALAndFull(t *testing.T) {
	s := open(t)
	var jm string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&jm); err != nil || jm != "wal" {
		t.Fatalf("journal_mode=%q err=%v", jm, err)
	}
	var sync int
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil || sync != 2 {
		t.Fatalf("synchronous=%d err=%v", sync, err)
	}
}

func TestAddIfAbsentNeverOverwritesAndAddWins(t *testing.T) {
	s := open(t)
	exact := model.Span{TraceID: "t", SpanID: "root", Service: "gateway", Name: "request", StartMS: 1000, EndMS: 2000, ParentID: "harness1", Attrs: model.Attrs{"model": "m"}}
	coarse := model.Span{TraceID: "t", SpanID: "root", Service: "gateway", Name: "request", StartMS: 1000, EndMS: 2000, Attrs: model.Attrs{"model": "m"}}
	// the log's version first, then the service's own: the exact one replaces it
	s.AddIfAbsent([]model.Span{coarse})
	s.Add([]model.Span{exact})
	sp, _ := s.Trace("t")
	if sp[0].ParentID != "harness1" {
		t.Fatalf("exact must replace: %+v", sp[0])
	}
	// the service's own first, then the log's: the log's is ignored
	s.AddIfAbsent([]model.Span{coarse})
	sp, _ = s.Trace("t")
	if sp[0].ParentID != "harness1" {
		t.Fatalf("log must not overwrite: %+v", sp[0])
	}
	// a span the log knows and the service did not post is still added
	touched, _ := s.AddIfAbsent([]model.Span{{TraceID: "t", SpanID: "reply", ParentID: "root", Service: "gateway", Name: "reply", StartMS: 1990, EndMS: 2000}})
	sp, _ = s.Trace("t")
	if len(sp) != 2 || len(touched) != 1 {
		t.Fatalf("%d spans, touched %v", len(sp), touched)
	}
}
