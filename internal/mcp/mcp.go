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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const protocolVersion = "2025-06-18"

// The limits that keep one request, one answer or one slow call from taking the server down.
const (
	maxLine      = 1 << 20 // bytes in one request line; a longer one is answered with an error and skipped
	maxAnswer    = 8 << 20 // bytes read of one answer from llm-hops
	maxArg       = 200     // bytes in a string argument (the API's own limit for a filter)
	maxTraces    = 100     // rows hops_traces and hops_slowest print at most; offset pages on
	defaultSpans = 200     // spans hops_trace prints unless max_spans says otherwise
	maxSpansCap  = 1000
	maxWindowMS  = 3650 * 86_400_000
	workers      = 8 // tool calls running against llm-hops at once; the rest wait their turn
)

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

var windowProp = map[string]any{"type": "string", "description": "how far back: 15m, 1h, 6h, 24h, 7d or any number of m, h or d (default 1h), or a number of milliseconds"}

// Tools is what the server offers.
var Tools = []Tool{
	{Name: "hops_stats", Description: "Request counts, error rate, p50/p95, model switches, time per hop, per model and local-against-cloud for a window. Start here for 'how is the system doing?'",
		InputSchema: obj(map[string]any{"window": windowProp})},
	{Name: "hops_traces", Description: "The newest requests in a window, filterable: model, client, service, status (ok|error), min_ms (slower than), q (free text). Returns summaries with trace ids, at most 100 a call, and says how many more match; offset pages through them.",
		InputSchema: obj(map[string]any{"window": windowProp, "model": str("served model"), "client": str("client name"), "service": str("first service, e.g. gateway or harness"),
			"status": str("ok or error"), "min_ms": num("only requests slower than this many milliseconds"), "q": str("free text over the attributes"),
			"limit": num("at most this many (default 20, at most 100)"), "offset": num("skip this many, for the next page (default 0)")})},
	{Name: "hops_trace", Description: "One request in full: every hop with its timing and attributes, as a waterfall in text. Use the trace id from hops_traces. A very large trace is cut to its first max_spans spans, with a note.",
		InputSchema: obj(map[string]any{"trace_id": str("the trace id"), "max_spans": num("print at most this many spans, by start time (default 200, at most 1000)")}, "trace_id")},
	{Name: "hops_slowest", Description: "The slowest requests of the whole window, slowest first, with where their time went, for 'why was it slow?'",
		InputSchema: obj(map[string]any{"window": windowProp, "limit": num("how many (default 5, at most 100)")})},
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

var null = json.RawMessage("null")

// session is one Serve: the output, and the calls in flight by request id so a cancellation finds them.
type session struct {
	wmu  sync.Mutex
	enc  *json.Encoder
	werr error

	mu       sync.Mutex
	inFlight map[string]*flight
	sem      chan struct{}
	wg       sync.WaitGroup
}

type flight struct {
	cancel    context.CancelFunc
	cancelled bool // by the client: it does not want the answer any more
}

func (ss *session) write(resp response) {
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	if ss.werr == nil {
		ss.werr = ss.enc.Encode(resp)
	}
}

func (ss *session) writeErr() error {
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	return ss.werr
}

// Serve reads requests from r and writes responses to w until r ends, then waits for the calls still
// running. Tool calls run beside the reader, so a slow one does not hold up a ping or another call, and
// their answers come in the order they finish (the id says which is which).
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	if s.Client == nil {
		s.Client = &http.Client{Timeout: 20 * time.Second}
	}
	ss := &session{enc: json.NewEncoder(w), inFlight: map[string]*flight{}, sem: make(chan struct{}, workers)}
	br := bufio.NewReaderSize(r, 64<<10)
	var err error
	for err == nil && ss.writeErr() == nil {
		var line []byte
		var tooLong bool
		line, tooLong, err = readLine(br, maxLine)
		if tooLong {
			ss.write(response{JSONRPC: "2.0", ID: null, Error: &rpcError{-32600, fmt.Sprintf("the request is too large: a line is at most %d bytes", maxLine)}})
			continue
		}
		if line = bytes.TrimSpace(line); len(line) > 0 {
			s.dispatch(ss, line)
		}
	}
	ss.wg.Wait()
	if werr := ss.writeErr(); werr != nil {
		return werr
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// readLine returns the next line without its newline. A line over max bytes is read to its end and
// dropped (tooLong), so the stream stays in step and the memory stays bounded.
func readLine(br *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > max+1 { // +1: the newline
				line, tooLong = nil, true
			} else {
				line = append(line, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, tooLong, err
	}
}

// dispatch answers one line: the quick methods here, a tool call in its own goroutine.
func (s *Server) dispatch(ss *session, line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		if json.Valid(line) { // JSON, but not a request object (a batch, a string ...)
			ss.write(response{JSONRPC: "2.0", ID: null, Error: &rpcError{-32600, "invalid request: one JSON-RPC object a line (batches are not supported)"}})
		} else {
			ss.write(response{JSONRPC: "2.0", ID: null, Error: &rpcError{-32700, "parse error"}})
		}
		return
	}
	if req.ID != nil && !validID(req.ID) {
		ss.write(response{JSONRPC: "2.0", ID: null, Error: &rpcError{-32600, "invalid request: id must be a string or a number"}})
		return
	}
	if req.ID == nil { // a notification: nothing to answer
		if req.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &p) == nil {
				ss.cancel(string(bytes.TrimSpace(p.RequestID)))
			}
		}
		return
	}
	if req.Method != "tools/call" {
		ss.write(s.handle(context.Background(), req))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &flight{cancel: cancel}
	key := string(req.ID)
	ss.mu.Lock()
	ss.inFlight[key] = f
	ss.mu.Unlock()
	ss.wg.Add(1)
	go func() {
		defer ss.wg.Done()
		defer cancel()
		var resp response
		select {
		case ss.sem <- struct{}{}:
			resp = s.handle(ctx, req)
			<-ss.sem
		case <-ctx.Done():
		}
		ss.mu.Lock()
		if ss.inFlight[key] == f {
			delete(ss.inFlight, key)
		}
		cancelled := f.cancelled
		ss.mu.Unlock()
		if !cancelled { // the protocol: no answer to a cancelled request
			ss.write(resp)
		}
	}()
}

func (ss *session) cancel(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if f := ss.inFlight[id]; f != nil {
		f.cancelled = true
		f.cancel()
	}
}

// validID: JSON-RPC allows a string, a number or null.
func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	switch c := id[0]; {
	case c == '"', c == '-', c >= '0' && c <= '9':
		return true
	}
	return string(id) == "null"
}

func (s *Server) handle(ctx context.Context, req request) response {
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
		if err := json.Unmarshal(req.Params, &p); err != nil {
			out.Error = &rpcError{-32602, `invalid params: tools/call takes {"name": "...", "arguments": {...}}`}
			return out
		}
		text, err := s.call(ctx, p.Name, p.Arguments)
		if err != nil {
			out.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}}, "isError": true}
		} else {
			out.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		}
	case "":
		out.Error = &rpcError{-32600, "invalid request: no method"}
	default:
		out.Error = &rpcError{-32601, "method not found: " + cutText(req.Method, 80)}
	}
	return out
}

// Call runs one tool and returns its text; exported for tests.
func (s *Server) Call(name string, args map[string]any) (string, error) {
	return s.call(context.Background(), name, args)
}

// number reads a numeric argument; a client that sends "5" for 5 is understood.
func number(args map[string]any, key string) (float64, bool, error) {
	switch t := args[key].(type) {
	case nil:
		return 0, false, nil
	case float64:
		return t, true, nil
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f, true, nil
		}
	}
	return 0, false, fmt.Errorf("%s must be a number", key)
}

// count is a numeric argument as a whole number between 0 and most; absent or not positive gives def.
func count(args map[string]any, key string, def, most int) (int, error) {
	v, ok, err := number(args, key)
	if err != nil {
		return 0, err
	}
	if !ok || !(v >= 1) {
		return def, nil
	}
	if v > float64(most) {
		return most, nil
	}
	return int(v), nil
}

// text reads a string argument, refusing what no filter needs: over maxArg bytes, or not text.
func text(args map[string]any, key string) (string, error) {
	switch t := args[key].(type) {
	case nil:
		return "", nil
	case string:
		if len(t) > maxArg {
			return "", fmt.Errorf("%s is too long: at most %d bytes", key, maxArg)
		}
		if !utf8.ValidString(t) || strings.ContainsRune(t, 0) {
			return "", fmt.Errorf("%s is not plain text", key)
		}
		return t, nil
	}
	return "", fmt.Errorf("%s must be a string", key)
}

func (s *Server) call(ctx context.Context, name string, args map[string]any) (string, error) {
	since, until := window(args["window"])
	q := url.Values{}
	q.Set("since", strconv.FormatInt(since, 10))
	q.Set("until", strconv.FormatInt(until, 10))
	switch name {
	case "hops_stats":
		var st statsOut
		if err := s.get(ctx, "/api/v1/stats", q, &st); err != nil {
			return "", err
		}
		return st.text(), nil
	case "hops_flow":
		var f flowOut
		if err := s.get(ctx, "/api/v1/flow", q, &f); err != nil {
			return "", err
		}
		return f.text(), nil
	case "hops_traces", "hops_slowest":
		for _, k := range []string{"model", "client", "service", "status", "q"} {
			v, err := text(args, k)
			if err != nil {
				return "", err
			}
			if v != "" {
				q.Set(k, v)
			}
		}
		if v, _, err := number(args, "min_ms"); err != nil {
			return "", err
		} else if v >= 1 {
			q.Set("min_ms", strconv.FormatInt(int64(min(v, 1e15)), 10))
		}
		def := 20
		if name == "hops_slowest" {
			def = 5
			q.Set("sort", "duration") // ranked by the server over the whole window, not over its newest rows
		}
		limit, err := count(args, "limit", def, maxTraces)
		if err != nil {
			return "", err
		}
		offset, err := count(args, "offset", 0, 1<<30)
		if err != nil {
			return "", err
		}
		q.Set("limit", strconv.Itoa(limit))
		q.Set("offset", strconv.Itoa(offset))
		q.Set("count", "1")
		var t tracesOut
		if err := s.get(ctx, "/api/v1/traces", q, &t); err != nil {
			return "", err
		}
		if name == "hops_slowest" {
			t.sortByDuration() // a server before sort=duration answers newest first
		}
		if len(t.Traces) > limit {
			t.Traces = t.Traces[:limit]
		}
		return t.text(offset, name == "hops_slowest"), nil
	case "hops_trace":
		id, err := text(args, "trace_id")
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", fmt.Errorf("trace_id is required")
		}
		spans, err := count(args, "max_spans", defaultSpans, maxSpansCap)
		if err != nil {
			return "", err
		}
		var tr traceOut
		if err := s.get(ctx, "/api/v1/traces/"+url.PathEscape(id), url.Values{"max_spans": {strconv.Itoa(spans)}}, &tr); err != nil {
			return "", err
		}
		return tr.text(spans), nil
	}
	return "", fmt.Errorf("unknown tool %q", cutText(name, 80))
}

func (s *Server) get(ctx context.Context, path string, q url.Values, into any) error {
	if s.Client == nil {
		s.Client = &http.Client{Timeout: 20 * time.Second}
	}
	u := strings.TrimRight(s.Base, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
		if resp.StatusCode >= 500 { // its reason is in the server's log, not for the assistant
			return fmt.Errorf("llm-hops answered %s", resp.Status)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("llm-hops answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return fmt.Errorf("llm-hops at %s: %w", s.Base, err)
	}
	if len(body) > maxAnswer {
		return fmt.Errorf("the answer from llm-hops is over %d MB: narrow the window, or lower limit or max_spans", maxAnswer>>20)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("%s did not answer as llm-hops does (is -to the llm-hops server?)", s.Base)
	}
	return nil
}

// window turns the argument into since and until. What it cannot read is the default hour; a window is
// at most ten years.
func window(v any) (int64, int64) {
	now := time.Now().UnixMilli()
	d := int64(3_600_000)
	switch t := v.(type) {
	case float64:
		if t >= 1 {
			d = int64(min(t, maxWindowMS))
		}
	case string:
		t = strings.TrimSpace(t)
		unit := int64(1)
		if n := len(t); n > 1 {
			switch t[n-1] {
			case 'm':
				unit, t = 60_000, t[:n-1]
			case 'h':
				unit, t = 3_600_000, t[:n-1]
			case 'd':
				unit, t = 86_400_000, t[:n-1]
			}
		}
		if n, err := strconv.ParseInt(t, 10, 64); err == nil && n > 0 {
			d = maxWindowMS
			if n < maxWindowMS/unit {
				d = n * unit
			}
		}
	}
	return now - d, now + 60_000
}
