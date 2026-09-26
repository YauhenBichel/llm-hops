// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package mcp is a Model Context Protocol server over stdio: an assistant (Claude Code, Cursor, any MCP
// client) gets tools to ask a running llm-hops server what happened. It speaks JSON-RPC 2.0, one message per
// line, the MCP methods initialize, tools/list and tools/call, and talks to llm-hops over its HTTP API, so it
// runs wherever the client runs.
//
//	claude mcp add llm-hops -- llm-hops mcp -to http://127.0.0.1:11602
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const protocolVersion = "2025-06-18"

// Server answers MCP requests by calling the llm-hops API.
type Server struct {
	Base    string
	Token   string
	Version string
	Client  *http.Client
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Tool is one entry of tools/list.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var windowProp = map[string]any{"type": "string", "description": "how far back: 15m, 1h, 6h, 24h, 7d (default 1h), or a number of milliseconds"}

// Tools is what the server offers.
var Tools = []Tool{
	{Name: "hops_stats", Description: "Request counts, error rate, p50/p95, model switches, time per hop, per model and local-against-cloud for a window. Start here for 'how is the system doing?'",
		InputSchema: obj(map[string]any{"window": windowProp})},
	{Name: "hops_traces", Description: "The newest requests in a window, filterable: model, client, service, status (ok|error), min_ms (slower than), q (free text). Returns summaries with trace ids.",
		InputSchema: obj(map[string]any{"window": windowProp, "model": str("served model"), "client": str("client name"), "service": str("first service, e.g. gateway or harness"),
			"status": str("ok or error"), "min_ms": num("only requests slower than this many milliseconds"), "q": str("free text over the attributes"), "limit": num("at most this many (default 20)")})},
	{Name: "hops_trace", Description: "One request in full: every hop with its timing and attributes, as a waterfall in text. Use the trace id from hops_traces.",
		InputSchema: obj(map[string]any{"trace_id": str("the trace id")}, "trace_id")},
	{Name: "hops_slowest", Description: "The slowest requests in a window with where their time went, for 'why was it slow?'",
		InputSchema: obj(map[string]any{"window": windowProp, "limit": num("how many (default 5)")})},
	{Name: "hops_flow", Description: "The services seen in a window and the hops between them with counts and average times: the shape of the system.",
		InputSchema: obj(map[string]any{"window": windowProp})},
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
func num(d string) map[string]any { return map[string]any{"type": "number", "description": d} }

// Serve reads requests from r and writes responses to w until r ends.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	if s.Client == nil {
		s.Client = &http.Client{Timeout: 20 * time.Second}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(response{JSONRPC: "2.0", Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if req.ID == nil { // a notification: nothing to answer
			continue
		}
		resp := s.handle(req)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(req request) response {
	out := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		out.Result = map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo":   map[string]any{"name": "llm-hops", "version": s.Version},
			"instructions": "Tools over a llm-hops server: what the local LLM system did, request by request. Start with hops_stats; drill in with hops_slowest, hops_traces and hops_trace."}
	case "ping":
		out.Result = map[string]any{}
	case "tools/list":
		out.Result = map[string]any{"tools": Tools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		text, err := s.call(p.Name, p.Arguments)
		if err != nil {
			out.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}}, "isError": true}
		} else {
			out.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		}
	default:
		out.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return out
}

// Call runs one tool and returns its text; exported for tests.
func (s *Server) Call(name string, args map[string]any) (string, error) { return s.call(name, args) }

func (s *Server) call(name string, args map[string]any) (string, error) {
	since, until := window(args["window"])
	q := url.Values{}
	q.Set("since", strconv.FormatInt(since, 10))
	q.Set("until", strconv.FormatInt(until, 10))
	switch name {
	case "hops_stats":
		var st statsOut
		if err := s.get("/api/v1/stats", q, &st); err != nil {
			return "", err
		}
		return st.text(), nil
	case "hops_flow":
		var f flowOut
		if err := s.get("/api/v1/flow", q, &f); err != nil {
			return "", err
		}
		return f.text(), nil
	case "hops_traces", "hops_slowest":
		for _, k := range []string{"model", "client", "service", "status", "q"} {
			if v, ok := args[k].(string); ok && v != "" {
				q.Set(k, v)
			}
		}
		if v, ok := args["min_ms"].(float64); ok && v > 0 {
			q.Set("min_ms", strconv.FormatInt(int64(v), 10))
		}
		limit := 20
		if name == "hops_slowest" {
			limit = 5
			q.Set("limit", "2000")
		}
		if v, ok := args["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		if name == "hops_traces" {
			q.Set("limit", strconv.Itoa(limit))
		}
		var t tracesOut
		if err := s.get("/api/v1/traces", q, &t); err != nil {
			return "", err
		}
		if name == "hops_slowest" {
			t.sortByDuration()
			if len(t.Traces) > limit {
				t.Traces = t.Traces[:limit]
			}
		}
		return t.text(), nil
	case "hops_trace":
		id, _ := args["trace_id"].(string)
		if id == "" {
			return "", fmt.Errorf("trace_id is required")
		}
		var tr traceOut
		if err := s.get("/api/v1/traces/"+url.PathEscape(id), nil, &tr); err != nil {
			return "", err
		}
		return tr.text(), nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func (s *Server) get(path string, q url.Values, into any) error {
	if s.Client == nil {
		s.Client = &http.Client{Timeout: 20 * time.Second}
	}
	u := strings.TrimRight(s.Base, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("llm-hops at %s: %w", s.Base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("llm-hops answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func window(v any) (int64, int64) {
	now := time.Now().UnixMilli()
	d := int64(3_600_000)
	switch t := v.(type) {
	case float64:
		if t > 0 {
			d = int64(t)
		}
	case string:
		switch strings.TrimSpace(t) {
		case "", "1h":
		case "15m":
			d = 900_000
		case "6h":
			d = 21_600_000
		case "24h", "1d":
			d = 86_400_000
		case "7d":
			d = 604_800_000
		default:
			if n, err := strconv.ParseInt(t, 10, 64); err == nil && n > 0 {
				d = n
			}
		}
	}
	return now - d, now + 60_000
}
