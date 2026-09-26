// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YauhenBichel/llm-hops/internal/demo"
	"github.com/YauhenBichel/llm-hops/internal/model"
	"github.com/YauhenBichel/llm-hops/internal/server"
	"github.com/YauhenBichel/llm-hops/internal/store"
)

func hops(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	g := demo.New(3, model.NowMS()-600_000, 40, 600)
	var slowest string
	var slowestMS int64
	for i := 0; i < 40; i++ {
		sp := g.Next()
		st.Add(sp)
		for _, s := range sp {
			if s.ParentID == "" && s.EndMS-s.StartMS > slowestMS {
				slowestMS, slowest = s.EndMS-s.StartMS, s.TraceID
			}
		}
	}
	srv := server.New(st, server.NewBroadcast())
	srv.Token = "tok"
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, slowest
}

func TestJSONRPCConversation(t *testing.T) {
	ts, slowest := hops(t)
	s := &Server{Base: ts.URL, Token: "tok", Version: "test"}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hops_stats","arguments":{"window":"1h"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"hops_slowest","arguments":{"limit":2}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"hops_trace","arguments":{"trace_id":"` + slowest + `"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"hops_flow","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"resources/list"}`,
		`not json`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := s.Serve(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 9 { // 8 answers (the notification gets none) + the parse error
		t.Fatalf("%d lines:\n%s", len(lines), out.String())
	}
	var init map[string]any
	json.Unmarshal([]byte(lines[0]), &init)
	if init["result"].(map[string]any)["protocolVersion"] != protocolVersion {
		t.Fatalf("initialize: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"hops_stats"`) || !strings.Contains(lines[1], `"hops_trace"`) {
		t.Fatalf("tools/list: %s", lines[1])
	}
	text := func(line string) string {
		var r map[string]any
		json.Unmarshal([]byte(line), &r)
		return r["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	if st := text(lines[2]); !strings.Contains(st, "Window: 40 requests") || !strings.Contains(st, "Time per hop") {
		t.Fatalf("stats: %s", st)
	}
	if sl := text(lines[3]); strings.Count(sl, "id=") != 2 || !strings.Contains(sl, slowest) {
		t.Fatalf("slowest: %s", sl)
	}
	if tr := text(lines[4]); !strings.Contains(tr, "Trace "+slowest) || !strings.Contains(tr, "request") {
		t.Fatalf("trace: %s", tr)
	}
	if fl := text(lines[5]); !strings.Contains(fl, "Services:") || !strings.Contains(fl, "gateway") {
		t.Fatalf("flow: %s", fl)
	}
	if !strings.Contains(lines[6], `"isError":true`) {
		t.Fatalf("unknown tool: %s", lines[6])
	}
	if !strings.Contains(lines[7], `-32601`) {
		t.Fatalf("unknown method: %s", lines[7])
	}
	if !strings.Contains(lines[8], `-32700`) {
		t.Fatalf("parse error: %s", lines[8])
	}
}

func TestWrongTokenIsAToolError(t *testing.T) {
	ts, _ := hops(t)
	s := &Server{Base: ts.URL, Token: "wrong", Version: "test"}
	if _, err := s.Call("hops_stats", nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err=%v", err)
	}
}

func TestWindows(t *testing.T) {
	for in, want := range map[any]int64{"15m": 900_000, "1h": 3_600_000, "6h": 21_600_000, "24h": 86_400_000, "7d": 604_800_000, "": 3_600_000,
		"120000": 120_000, float64(5000): 5000, "junk": 3_600_000} {
		since, until := window(in)
		if until-since != want+60_000 {
			t.Fatalf("%v: %d", in, until-since)
		}
	}
}
