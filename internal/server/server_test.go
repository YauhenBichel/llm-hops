// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"encoding/json"
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
