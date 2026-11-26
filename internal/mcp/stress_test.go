// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/model"
	"github.com/YauhenBichel/llm-hops/internal/server"
	"github.com/YauhenBichel/llm-hops/internal/store"
)

// The tests for what the stress test of November 2026 found: each one failed before its fix.

func over(t *testing.T, spans []model.Span) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Add(spans); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st, server.NewBroadcast()))
	t.Cleanup(ts.Close)
	return &Server{Base: ts.URL, Version: "test"}
}

func roots(n int, startMS int64, durMS func(i int) int64) []model.Span {
	out := make([]model.Span, n)
	for i := range out {
		start := startMS + int64(i)*10
		out[i] = model.Span{TraceID: fmt.Sprintf("t%05d", i), SpanID: "r", Service: "gateway", Name: "request", StartMS: start, EndMS: start + durMS(i),
			Status: "ok", Attrs: model.Attrs{"model": "m", "client": "c"}}
	}
	return out
}

// talk runs one conversation to its end and returns the answers by id ("null" for the ones without).
func talk(t *testing.T, s *Server, lines ...string) map[string]string {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("not a JSON line: %.200s", l)
		}
		if _, twice := got[string(r.ID)]; twice {
			t.Fatalf("two answers for id %s", r.ID)
		}
		got[string(r.ID)] = l
	}
	return got
}

func TestSlowestLooksAtTheWholeWindowNotTheNewestRows(t *testing.T) {
	now := model.NowMS()
	// one slow request three hours ago, then 2 500 quick ones: more than the 2 000 newest the tool used to rank
	spans := roots(2500, now-600_000, func(int) int64 { return 5 })
	spans = append(spans, model.Span{TraceID: "the-slow-one", SpanID: "r", Service: "gateway", Name: "request", StartMS: now - 3*3_600_000,
		EndMS: now - 3*3_600_000 + 90_000, Status: "ok", Attrs: model.Attrs{"model": "m"}})
	s := over(t, spans)
	out, err := s.Call("hops_slowest", map[string]any{"window": "6h", "limit": 1.0})
	if err != nil || !strings.Contains(out, "id=the-slow-one") || strings.Count(out, "id=") != 1 {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if !strings.Contains(out, "of 2501, slowest first") {
		t.Fatalf("the answer must say how many it ranked:\n%s", out)
	}
}

func TestAHugeTraceIsPrintedInPartWithANote(t *testing.T) {
	const n = 20_000
	now := model.NowMS()
	spans := make([]model.Span, n)
	for i := range spans { // a chain: every span the child of the one before, the worst case for the old indent walk
		spans[i] = model.Span{TraceID: "big", SpanID: fmt.Sprintf("s%d", i), Service: "svc", Name: "step", StartMS: now - 60_000 + int64(i), EndMS: now - 60_000 + int64(i) + 1,
			Status: "ok", Attrs: model.Attrs{"note": strings.Repeat("x", 500)}}
		if i > 0 {
			spans[i].ParentID = fmt.Sprintf("s%d", i-1)
		}
	}
	s := over(t, spans)
	begin := time.Now()
	out, err := s.Call("hops_trace", map[string]any{"trace_id": "big"})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("took %s (before: 19 s)", took)
	}
	if len(out) > 200_000 {
		t.Fatalf("%d bytes of text (before: 102 MB)", len(out))
	}
	if !strings.Contains(out, "20000 spans") || !strings.Contains(out, "Truncated: the first 200 of 20000 spans by start time, 19800 more not shown") {
		t.Fatalf("no note about the cut:\n%s", out[len(out)-300:])
	}
	if strings.Contains(out, strings.Repeat("x", maxAttrValue+1)) {
		t.Fatal("an attribute value was printed whole")
	}
	out, err = s.Call("hops_trace", map[string]any{"trace_id": "big", "max_spans": 50.0})
	if err != nil || !strings.Contains(out, "the first 50 of 20000") || strings.Count(out, "\n") != 52 {
		t.Fatalf("max_spans=50: err=%v lines=%d", err, strings.Count(out, "\n"))
	}
	if out, _ = s.Call("hops_trace", map[string]any{"trace_id": "big", "max_spans": 1e9}); !strings.Contains(out, "the first 1000 of 20000") {
		t.Fatalf("max_spans is capped at 1000:\n%s", out[len(out)-200:])
	}
}

func TestASpanWithManyAttributesIsCut(t *testing.T) {
	attrs := model.Attrs{}
	for i := 0; i < 300; i++ {
		attrs[fmt.Sprintf("key%03d", i)] = strings.Repeat("v", 40)
	}
	now := model.NowMS()
	s := over(t, []model.Span{{TraceID: "wide", SpanID: "r", Service: "svc", Name: "request", StartMS: now - 1000, EndMS: now, Status: "ok", Attrs: attrs}})
	out, err := s.Call("hops_trace", map[string]any{"trace_id": "wide"})
	if err != nil || len(out) > 2000 || !strings.Contains(out, "more attributes)") {
		t.Fatalf("err=%v %d bytes:\n%.300s", err, len(out), out)
	}
}

func TestTracesAreCappedAndPaged(t *testing.T) {
	s := over(t, roots(250, model.NowMS()-600_000, func(int) int64 { return 5 }))
	out, err := s.Call("hops_traces", map[string]any{"limit": 5000.0})
	if err != nil || strings.Count(out, "id=") != 100 {
		t.Fatalf("limit=5000: err=%v rows=%d", err, strings.Count(out, "id="))
	}
	if !strings.Contains(out, "Showing 1-100 of 250, newest first. 150 more match: call again with offset=100.") {
		t.Fatalf("no way on from the first page:\n%s", out[len(out)-200:])
	}
	if !strings.Contains(out, "id=t00249") || strings.Contains(out, "id=t00149\n") {
		t.Fatalf("the first page is the newest 100")
	}
	out, _ = s.Call("hops_traces", map[string]any{"limit": 100.0, "offset": 200.0})
	if strings.Count(out, "id=") != 50 || !strings.Contains(out, "Showing 201-250 of 250, newest first.\n") || !strings.Contains(out, "id=t00000") {
		t.Fatalf("the last page:\n%s", out[len(out)-200:])
	}
	if out, _ = s.Call("hops_traces", map[string]any{"offset": 900.0}); !strings.Contains(out, "No more requests: 250 match") {
		t.Fatalf("past the end: %s", out)
	}
	if _, err = s.Call("hops_traces", map[string]any{"limit": "many"}); err == nil || !strings.Contains(err.Error(), "limit must be a number") {
		t.Fatalf("limit=many: %v", err)
	}
}

func TestALineTooLongIsRefusedAndTheServerGoesOn(t *testing.T) {
	s := over(t, nil)
	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hops_traces","arguments":{"q":"` + strings.Repeat("a", 17<<20) + `"}}}`
	got := talk(t, s, huge, `{"jsonrpc":"2.0","id":2,"method":"ping"}`) // before: Serve returned "token too long" and the process ended
	if len(got) != 2 || !strings.Contains(got["null"], "-32600") || !strings.Contains(got["null"], "too large") {
		t.Fatalf("the long line: %v", got)
	}
	if !strings.Contains(got["2"], `"result":{}`) {
		t.Fatalf("the ping after it: %v", got)
	}
}

func TestStringArgumentsHaveALimitAndUpstreamErrorsAreNotPassedOn(t *testing.T) {
	s := over(t, nil)
	_, err := s.Call("hops_traces", map[string]any{"q": strings.Repeat("%", 60_000)})
	if err == nil || !strings.Contains(err.Error(), "q is too long: at most 200 bytes") {
		t.Fatalf("a 60 000-byte q: %v", err)
	}
	if _, err = s.Call("hops_traces", map[string]any{"model": 5.0}); err == nil || !strings.Contains(err.Error(), "model must be a string") {
		t.Fatalf("model=5: %v", err)
	}
	if _, err = s.Call("hops_trace", map[string]any{"trace_id": strings.Repeat("x", 500)}); err == nil || !strings.Contains(err.Error(), "trace_id is too long") {
		t.Fatalf("a long trace id: %v", err)
	}
	// an llm-hops that fails says so in its own log; its words do not reach the assistant
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "LIKE or GLOB pattern too complex", 500)
	}))
	defer up.Close()
	_, err = (&Server{Base: up.URL}).Call("hops_stats", nil)
	if err == nil || strings.Contains(err.Error(), "LIKE") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("a 500 from llm-hops: %v", err)
	}
}

func TestAnAnswerTooLargeIsAnErrorNotAnAllocation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"traces":[],"pad":"`))
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 64; i++ { // 64 MB if the reader took it all
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		w.Write([]byte(`"}`))
	}))
	defer up.Close()
	_, err := (&Server{Base: up.URL}).Call("hops_traces", nil)
	if err == nil || !strings.Contains(err.Error(), "over 8 MB") {
		t.Fatalf("err=%v", err)
	}
}

// A slow call must not hold up the next request, and a cancellation must reach llm-hops.
func TestCallsRunSideBySideAndCanBeCancelled(t *testing.T) {
	arrived, gone := make(chan struct{}, 1), make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/stats" {
			w.Write([]byte(`{"services":[],"edges":[]}`))
			return
		}
		arrived <- struct{}{}
		select { // a query that never ends by itself
		case <-r.Context().Done():
			gone <- struct{}{}
		case <-time.After(30 * time.Second):
		}
	}))
	defer up.Close()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- (&Server{Base: up.URL}).Serve(inR, outW); outW.Close() }()
	answers := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			answers <- sc.Text()
		}
		close(answers)
	}()
	wait := func(what string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	next := func(what string) string {
		t.Helper()
		select {
		case l := <-answers:
			return l
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
			return ""
		}
	}
	fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hops_stats","arguments":{}}}`)
	wait("the slow call to reach llm-hops", arrived)
	fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if l := next("the ping behind a slow call"); !strings.Contains(l, `"id":2`) {
		t.Fatalf("expected the ping's answer, got %s", l)
	}
	fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hops_flow","arguments":{}}}`)
	if l := next("a second call behind a slow one"); !strings.Contains(l, `"id":3`) || !strings.Contains(l, "Services") {
		t.Fatalf("expected the flow, got %s", l)
	}
	fmt.Fprintln(inW, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"the user moved on"}}`)
	wait("the cancellation to reach llm-hops", gone)
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if l, more := <-answers; more { // the protocol: a cancelled request gets no answer
		t.Fatalf("an answer after the cancellation: %s", l)
	}
}

func TestErrorsWithoutARequestCarryANullID(t *testing.T) {
	s := over(t, nil)
	got := talk(t, s, `not json`)
	if !strings.Contains(got["null"], `"id":null`) || !strings.Contains(got["null"], "-32700") { // before: no id member at all
		t.Fatalf("parse error: %v", got)
	}
	for _, line := range []string{`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`, `{"jsonrpc":"2.0","id":[1],"method":"ping"}`, `{"jsonrpc":"2.0","id":true,"method":"ping"}`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, `"ping"`} {
		got = talk(t, s, line)
		if len(got) != 1 || !strings.Contains(got["null"], `"id":null`) || !strings.Contains(got["null"], "-32600") {
			t.Fatalf("%s: %v", line, got)
		}
	}
	got = talk(t, s, `{"jsonrpc":"2.0","id":"a","method":"ping"}`, `{"jsonrpc":"2.0","id":7}`, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":[1]}`)
	if !strings.Contains(got[`"a"`], `"result":{}`) || !strings.Contains(got["7"], "-32600") || !strings.Contains(got["8"], "-32602") {
		t.Fatalf("%v", got)
	}
}

func TestAWindowIsAtMostTenYears(t *testing.T) {
	for _, in := range []any{"99999999d", "9223372036854775807h", 1e30, "45m", "36h"} {
		since, until := window(in)
		if d := until - since - 60_000; d <= 0 || d > maxWindowMS {
			t.Fatalf("%v: %d ms", in, d)
		}
	}
	if since, until := window("45m"); until-since != 45*60_000+60_000 {
		t.Fatalf("45m: %d", until-since)
	}
}
