// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTokenGuardsTheAPIAndThePageGetsACookie(t *testing.T) {
	s, ts := newServer(t)
	s.Token = "s3cret"
	// without the token: the API says so, the health check and the page itself still answer
	r, _ := http.Get(ts.URL + "/api/v1/traces")
	if r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("api without token: %d", r.StatusCode)
	}
	r, _ = http.Post(ts.URL+"/api/v1/spans", "application/json", strings.NewReader(`[]`))
	if r.StatusCode != 401 {
		t.Fatalf("post without token: %d", r.StatusCode)
	}
	r, _ = http.Get(ts.URL + "/metrics")
	if r.StatusCode != 401 {
		t.Fatalf("metrics without token: %d", r.StatusCode)
	}
	r, _ = http.Get(ts.URL + "/api/v1/health")
	if r.StatusCode != 200 {
		t.Fatalf("health must stay open: %d", r.StatusCode)
	}
	r, _ = http.Get(ts.URL + "/")
	if r.StatusCode != 200 {
		t.Fatalf("the page is served so it can ask for the cookie: %d", r.StatusCode)
	}
	// a bearer header
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/spans", strings.NewReader(`[{"trace_id":"a","start_ms":1}]`))
	req.Header.Set("Authorization", "Bearer s3cret")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("with bearer: %d", r.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 401 {
		t.Fatalf("wrong bearer: %d", r.StatusCode)
	}
	// the session call turns the token into a cookie, which the stream and the API accept
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/session", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 || len(r.Cookies()) != 1 || !r.Cookies()[0].HttpOnly {
		t.Fatalf("session: %d %v", r.StatusCode, r.Cookies())
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/traces", nil)
	req.AddCookie(r.Cookies()[0])
	r2, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(r2.Body)
	if r2.StatusCode != 200 || !strings.Contains(string(body), `"traces"`) {
		t.Fatalf("with cookie: %d %s", r2.StatusCode, body)
	}
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/session", nil)
	req.Header.Set("Authorization", "Bearer nope")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 401 {
		t.Fatalf("session with the wrong token: %d", r.StatusCode)
	}
}

func TestMetricsExposition(t *testing.T) {
	_, ts := newServer(t)
	post(t, ts, `[{"trace_id":"m1","span_id":"r","service":"gateway","name":"request","start_ms":`+nowStr()+`,"end_ms":`+nowStr()+`,"attrs":{"model":"m"}},
		{"trace_id":"m1","span_id":"q","parent_id":"r","service":"gateway","name":"queue","start_ms":`+nowStr()+`,"end_ms":`+nowStr()+`},
		{"bad":1}]`)
	r, err := http.Get(ts.URL + "/metrics")
	if err != nil || r.StatusCode != 200 {
		t.Fatal(err, r)
	}
	body, _ := io.ReadAll(r.Body)
	text := string(body)
	for _, want := range []string{"llm_hops_spans_received_total 2", "llm_hops_spans_rejected_total 1", "llm_hops_requests_5m 1",
		`llm_hops_hop_ms{hop="queue",quantile="0.5"}`, `llm_hops_model_requests_5m{model="m"} 1`, "# TYPE llm_hops_request_ms gauge"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
		t.Fatal(r.Header.Get("Content-Type"))
	}
}

func nowStr() string { return itoa(nowMS()) }

func nowMS() int64 { return time.Now().UnixMilli() }

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
