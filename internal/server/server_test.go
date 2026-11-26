// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/store"
)

func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st, NewBroadcast())
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func post(t *testing.T, ts *httptest.Server, body string) (int, map[string]any) {
	t.Helper()
	r, err := http.Post(ts.URL+"/api/v1/spans", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func get(t *testing.T, ts *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	r, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func TestSpansInTracesOut(t *testing.T) {
	_, ts := newServer(t)
	code, out := post(t, ts, `[{"trace_id":"abc","span_id":"r","service":"gateway","name":"request","start_ms":1000,"end_ms":2000,"attrs":{"client":"editor","model":"m"}},
		{"trace_id":"abc","span_id":"c","parent_id":"r","service":"ollama","name":"upstream","start_ms":1100,"end_ms":1900},
		{"nonsense":true}]`)
	if code != 200 || out["accepted"] != float64(2) || out["rejected"] != float64(1) {
		t.Fatalf("%d %v", code, out)
	}
	code, out = get(t, ts, "/api/v1/traces?since=0")
	traces := out["traces"].([]any)
	if code != 200 || len(traces) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	tr := traces[0].(map[string]any)
	if tr["duration_ms"] != float64(1000) || tr["spans"] != float64(2) {
		t.Fatalf("%v", tr)
	}
	code, out = get(t, ts, "/api/v1/traces/abc")
	if code != 200 || len(out["spans"].([]any)) != 2 {
		t.Fatalf("%d %v", code, out)
	}
	code, _ = get(t, ts, "/api/v1/traces/nope")
	if code != 404 {
		t.Fatalf("missing trace: %d", code)
	}
	code, out = get(t, ts, "/api/v1/stats?since=0&until=10000")
	if code != 200 || out["requests"] != float64(1) {
		t.Fatalf("%d %v", code, out)
	}
	code, out = get(t, ts, "/api/v1/flow?since=0&until=10000")
	if code != 200 || len(out["edges"].([]any)) != 1 {
		t.Fatalf("%d %v", code, out)
	}
}

func TestWrappedBodyAndBadBody(t *testing.T) {
	_, ts := newServer(t)
	code, out := post(t, ts, `{"spans":[{"trace_id":"x","start_ms":5}]}`)
	if code != 200 || out["accepted"] != float64(1) {
		t.Fatalf("%d %v", code, out)
	}
	code, _ = post(t, ts, `{"not":"a list"}`)
	if code != 400 {
		t.Fatalf("bad body: %d", code)
	}
	code, _ = post(t, ts, `nonsense`)
	if code != 400 {
		t.Fatalf("not json: %d", code)
	}
}

func TestStreamAnnouncesNewTraces(t *testing.T) {
	_, ts := newServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	hello, _ := rd.ReadString('\n')
	if !strings.HasPrefix(hello, ": hello") {
		t.Fatalf("got %q", hello)
	}
	time.Sleep(20 * time.Millisecond)
	post(t, ts, `[{"trace_id":"live1","service":"gateway","name":"request","start_ms":1,"end_ms":9,"attrs":{"model":"m"}}]`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			var ev map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatal(err)
			}
			if ev["type"] != "trace" || ev["trace"].(map[string]any)["trace_id"] != "live1" {
				t.Fatalf("event: %v", ev)
			}
			return
		}
	}
	t.Fatal("no event")
}

func TestCloseStreamsEndsAnOpenStream(t *testing.T) {
	s, ts := newServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	if _, err := rd.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	s.CloseStreams()
	s.CloseStreams() // a second call does no harm
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, rd)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stream ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream stayed open after CloseStreams")
	}
}

func TestPageAndAssets(t *testing.T) {
	_, ts := newServer(t)
	for _, p := range []string{"/", "/ui/app.js", "/ui/app.css"} {
		r, err := http.Get(ts.URL + p)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, err, r)
		}
		r.Body.Close()
	}
	r, _ := http.Get(ts.URL + "/ui/../go.mod")
	if r.StatusCode != 404 {
		t.Fatalf("traversal: %d", r.StatusCode)
	}
	r, _ = http.Get(ts.URL + "/ui/nope.js")
	if r.StatusCode != 404 {
		t.Fatalf("missing asset: %d", r.StatusCode)
	}
}

func TestOTLPTracesArrive(t *testing.T) {
	_, ts := newServer(t)
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"agent"}}]},"scopeSpans":[{"spans":[
	  {"traceId":"abcdef0123456789abcdef0123456789","spanId":"1111111111111111","name":"chat","startTimeUnixNano":"1790409578748000000","endTimeUnixNano":"1790409579748000000",
	   "attributes":[{"key":"gen_ai.request.model","value":{"stringValue":"m"}},{"key":"gen_ai.usage.output_tokens","value":{"intValue":"9"}}]}]}]}]}`
	r, err := http.Post(ts.URL+"/v1/traces", "application/json", strings.NewReader(body))
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("%v %v", err, r)
	}
	code, out := get(t, ts, "/api/v1/traces/abcdef0123456789abcdef0123456789")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	tr := out["trace"].(map[string]any)
	if tr["service"] != "agent" || tr["duration_ms"] != float64(1000) || tr["attrs"].(map[string]any)["model"] != "m" {
		t.Fatalf("%v", tr)
	}
	r, _ = http.Post(ts.URL+"/v1/traces", "application/x-protobuf", strings.NewReader("x"))
	if r.StatusCode != 415 {
		t.Fatalf("protobuf must be refused with a hint: %d", r.StatusCode)
	}
	r, _ = http.Get(ts.URL + "/api/openapi.json")
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("openapi: %d", r.StatusCode)
	}
}

func TestIfAbsentFlagOnThePost(t *testing.T) {
	_, ts := newServer(t)
	post(t, ts, `[{"trace_id":"ia","span_id":"root","service":"gateway","name":"request","start_ms":1,"end_ms":9,"parent_id":"p1","attrs":{"model":"m"}}]`)
	code, out := post(t, ts, `{"spans":[{"trace_id":"ia","span_id":"root","service":"gateway","name":"request","start_ms":1,"end_ms":9,"attrs":{"model":"m"}}],"if_absent":true}`)
	if code != 200 || out["accepted"] != float64(1) {
		t.Fatalf("%d %v", code, out)
	}
	_, tr := get(t, ts, "/api/v1/traces/ia")
	if tr["spans"].([]any)[0].(map[string]any)["parent_id"] != "p1" {
		t.Fatalf("the log-derived span overwrote the service's: %v", tr)
	}
}

func TestLoadsEndpoint(t *testing.T) {
	_, ts := newServer(t)
	post(t, ts, `[{"trace_id":"ld","span_id":"ld","service":"ollama","name":"load","start_ms":10,"end_ms":20,"attrs":{"model":"m","kind":"load"}}]`)
	code, out := get(t, ts, "/api/v1/loads?since=0&until=100")
	if code != 200 || len(out["loads"].([]any)) != 1 {
		t.Fatalf("%d %v", code, out)
	}
}

func getText(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	r, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func TestALongIDIsRejectedWithAReasonNotShortened(t *testing.T) {
	_, ts := newServer(t)
	a, b := strings.Repeat("X", 64)+"A", strings.Repeat("X", 64)+"B"
	code, out := post(t, ts, `[{"trace_id":"`+a+`","span_id":"1","start_ms":1000,"end_ms":1001,"attrs":{"model":"A"}},
		{"trace_id":"`+b+`","span_id":"1","start_ms":1000,"end_ms":1002,"attrs":{"model":"B"}},
		{"trace_id":"`+strings.Repeat("X", 64)+`","span_id":"1","start_ms":1000,"end_ms":1003,"attrs":{"model":"C"}}]`)
	if code != 200 || out["accepted"] != 1.0 || out["rejected"] != 2.0 {
		t.Fatalf("%d %v", code, out)
	}
	if errs, _ := out["errors"].([]any); len(errs) != 1 || !strings.Contains(errs[0].(string), "trace_id is longer than 64 bytes") {
		t.Fatalf("the sender must be told why, once: %v", out["errors"])
	}
	// the 64-byte id is its own trace, not a merge of the three; the long ones are not there under any name
	_, tr := get(t, ts, "/api/v1/traces/"+strings.Repeat("X", 64))
	if spans, _ := tr["spans"].([]any); len(spans) != 1 || spans[0].(map[string]any)["attrs"].(map[string]any)["model"] != "C" {
		t.Fatalf("the trace with the 64-byte id: %v", tr)
	}
	if code, _ := get(t, ts, "/api/v1/traces/"+a); code != 404 {
		t.Fatalf("a rejected id answered %d", code)
	}
}

func TestBadTimesAndIDsAreRejectedWithReasons(t *testing.T) {
	_, ts := newServer(t)
	code, out := post(t, ts, `[{"trace_id":"neg","start_ms":-5,"end_ms":3},{"trace_id":"huge","start_ms":1e30},
		{"trace_id":"u\u0000","start_ms":1},{"trace_id":{"a":1},"start_ms":1},{"trace_id":"ok","start_ms":1000,"end_ms":1005}]`)
	if code != 200 || out["accepted"] != 1.0 || out["rejected"] != 4.0 {
		t.Fatalf("%d %v", code, out)
	}
	if errs, _ := out["errors"].([]any); len(errs) != 3 { // the two bad times share a reason
		t.Fatalf("reasons: %v", out["errors"])
	}
	_, st := get(t, ts, "/api/v1/stats?since=0&until=9999999999999")
	if st["requests"] != 1.0 || st["max_ms"] != 5.0 {
		t.Fatalf("the bad spans reached the statistics: %v", st)
	}
}

func TestTracesSlowestFirstPagedAndCounted(t *testing.T) {
	_, ts := newServer(t)
	var spans []string
	for i := 0; i < 30; i++ { // the oldest is the slowest
		spans = append(spans, `{"trace_id":"t`+itoa(int64(i))+`","span_id":"r","service":"gateway","name":"request","start_ms":`+itoa(int64(1000+i*1000))+
			`,"end_ms":`+itoa(int64(1000+i*1000+(30-i)*10))+`,"attrs":{"model":"m"}}`)
	}
	post(t, ts, "["+strings.Join(spans, ",")+"]")
	_, out := get(t, ts, "/api/v1/traces?sort=duration&limit=3&count=1")
	rows := out["traces"].([]any)
	if len(rows) != 3 || rows[0].(map[string]any)["trace_id"] != "t0" || rows[2].(map[string]any)["trace_id"] != "t2" || out["total"] != 30.0 {
		t.Fatalf("slowest first with a total: %v", out)
	}
	_, out = get(t, ts, "/api/v1/traces?limit=10&offset=25&count=1")
	if rows := out["traces"].([]any); len(rows) != 5 || rows[0].(map[string]any)["trace_id"] != "t4" || out["total"] != 30.0 {
		t.Fatalf("the last page, newest first: %v", out)
	}
	if _, out = get(t, ts, "/api/v1/traces"); out["total"] != nil {
		t.Fatalf("a total without count=1: %v", out["total"])
	}
	if code, body := getText(t, ts, "/api/v1/traces?sort=sideways"); code != 400 || !strings.Contains(body, "duration") {
		t.Fatalf("an unknown sort: %d %s", code, body)
	}
}

func TestALongFilterIsRefusedAndStoreErrorsStayInTheLog(t *testing.T) {
	s, ts := newServer(t)
	code, body := getText(t, ts, "/api/v1/traces?q="+strings.Repeat("x", 60_000))
	if code != 400 || !strings.Contains(body, "q is too long") || strings.Contains(body, "LIKE") {
		t.Fatalf("a 60 000-byte q: %d %s", code, body)
	}
	if code, _ := getText(t, ts, "/api/v1/traces?q="+strings.Repeat("x", 200)); code != 200 {
		t.Fatalf("a 200-byte q: %d", code)
	}
	s.Store.Close() // every query fails from here on
	for _, path := range []string{"/api/v1/traces", "/api/v1/stats", "/api/v1/flow", "/api/v1/facets", "/api/v1/loads", "/metrics", "/api/v1/traces/x"} {
		code, body := getText(t, ts, path)
		if code != 500 || strings.Contains(body, "sql") || strings.Contains(body, "database") {
			t.Fatalf("%s: %d %q", path, code, body)
		}
	}
}

func TestTheSpanCapIsReportedToTheSender(t *testing.T) {
	s, ts := newServer(t)
	s.Store.MaxSpansPerTrace = 3
	var spans []string
	for i := 0; i < 5; i++ {
		spans = append(spans, `{"trace_id":"big","span_id":"s`+itoa(int64(i))+`","start_ms":`+nowStr()+`}`)
	}
	code, out := post(t, ts, "["+strings.Join(spans, ",")+"]")
	if code != 200 || out["accepted"] != 3.0 || out["dropped"] != 2.0 {
		t.Fatalf("%d %v", code, out)
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "at most 3 spans") || !strings.Contains(note, `"big"`) {
		t.Fatalf("note: %v", out["note"])
	}
	_, tr := get(t, ts, "/api/v1/traces/big")
	if len(tr["spans"].([]any)) != 3 {
		t.Fatalf("stored: %v", tr)
	}
	// the first spans only, with the summary still saying how many there are
	_, tr = get(t, ts, "/api/v1/traces/big?max_spans=2")
	if len(tr["spans"].([]any)) != 2 || tr["trace"].(map[string]any)["spans"] != 3.0 {
		t.Fatalf("max_spans=2: %v", tr)
	}
	if _, body := getText(t, ts, "/metrics"); !strings.Contains(body, "llm_hops_spans_dropped_total 2") || !strings.Contains(body, "llm_hops_spans_received_total 3") {
		t.Fatalf("metrics: %s", body)
	}
	// OTLP says it the protocol's way
	otlp := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"big","spanId":"zz","name":"x","startTimeUnixNano":"` + nowStr() + `000000"}]}]}]}`
	r, err := http.Post(ts.URL+"/v1/traces", "application/json", strings.NewReader(otlp))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), `"rejectedSpans":1`) || !strings.Contains(string(b), "at most 3 spans") {
		t.Fatalf("OTLP: %s", b)
	}
}

func TestAnAbandonedRequestStopsItsQuery(t *testing.T) {
	s, _ := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, path := range []string{"/api/v1/stats", "/api/v1/flow", "/api/v1/traces", "/api/v1/traces/x", "/api/v1/facets", "/api/v1/loads", "/metrics"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil).WithContext(ctx))
		// the handler passes the request's context to the store, which refuses to start; nothing is written
		// for a caller that has gone (before: the query ran to its end and answered 200)
		if w.Body.Len() != 0 {
			t.Fatalf("%s answered a request that was gone: %d %s", path, w.Code, w.Body.String())
		}
	}
}
