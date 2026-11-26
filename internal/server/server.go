// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package server is the HTTP side: spans in, traces and statistics out, a live stream for the page, and
// the page itself.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/model"
	"github.com/YauhenBichel/llm-hops/internal/otlp"
	"github.com/YauhenBichel/llm-hops/internal/store"
	"github.com/YauhenBichel/llm-hops/ui"
)

const (
	maxBatch = 5000
	maxBody  = 32 << 20
	// maxParam is the longest filter value in a query string, in bytes: no model, client or search text
	// needs more, and SQLite refuses a LIKE pattern past 50 000.
	maxParam = 200
)

// Broadcast fans a small event out to every open stream. A slow reader loses events, never blocks the writer.
type Broadcast struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewBroadcast makes an empty one.
func NewBroadcast() *Broadcast { return &Broadcast{subs: map[chan []byte]struct{}{}} }

// Subscribe returns a channel that receives encoded events until Unsubscribe.
func (b *Broadcast) Subscribe() chan []byte {
	ch := make(chan []byte, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes the channel.
func (b *Broadcast) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// Listening says whether any stream is open.
func (b *Broadcast) Listening() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs) > 0
}

// Publish sends an event to every subscriber.
func (b *Broadcast) Publish(event any) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- data:
		default:
		}
	}
}

// Server serves the API and the page.
type Server struct {
	Store   *store.Store
	Bus     *Broadcast
	Title   string
	Token   string // when set, every call but the health check needs it (auth.go)
	started time.Time
	mux     *http.ServeMux

	spansReceived atomic.Int64
	spansRejected atomic.Int64

	closing   chan struct{}
	closeOnce sync.Once
}

// CloseStreams ends every open event stream. http.Server.Shutdown waits for handlers to return and
// a stream never returns by itself, so this is registered with RegisterOnShutdown.
func (s *Server) CloseStreams() {
	s.closeOnce.Do(func() { close(s.closing) })
}

// New wires the routes.
func New(st *store.Store, bus *Broadcast) *Server {
	s := &Server{Store: st, Bus: bus, Title: "llm-hops", started: time.Now(), mux: http.NewServeMux(), closing: make(chan struct{})}
	s.mux.HandleFunc("POST /api/v1/spans", s.postSpans)
	s.mux.HandleFunc("GET /api/v1/traces", s.getTraces)
	s.mux.HandleFunc("GET /api/v1/traces/{id}", s.getTrace)
	s.mux.HandleFunc("GET /api/v1/stats", s.getStats)
	s.mux.HandleFunc("GET /api/v1/flow", s.getFlow)
	s.mux.HandleFunc("GET /api/v1/facets", s.getFacets)
	s.mux.HandleFunc("GET /api/v1/loads", s.getLoads)
	s.mux.HandleFunc("GET /api/v1/stream", s.stream)
	s.mux.HandleFunc("GET /api/v1/health", s.health)
	s.mux.HandleFunc("POST /api/v1/session", s.setCookie)
	s.mux.HandleFunc("GET /metrics", s.metrics)
	s.mux.HandleFunc("POST /v1/traces", s.postOTLP)
	s.mux.HandleFunc("GET /api/openapi.json", s.openapi)
	s.mux.HandleFunc("GET /ui/{name}", s.uiFile)
	s.mux.HandleFunc("GET /{$}", s.index)
	return s
}

// ServeHTTP makes Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) && r.URL.Path != "/api/v1/session" && r.URL.Path != "/api/openapi.json" {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/metrics" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "a token is required (Authorization: Bearer ..., or open the page once as /#token=...)", http.StatusUnauthorized)
			return
		}
		// the page itself is served: it reads #token= from its address and asks for the cookie
	}
	s.mux.ServeHTTP(w, r)
}

// Publish announces the traces touched by an insert (used by the API and by the in-process adapters).
func (s *Server) Publish(touched []string) {
	if !s.Bus.Listening() { // nobody to tell: a sender does not pay for the read
		return
	}
	for _, id := range touched {
		if sum, err := s.Store.Written(id); err == nil && sum != nil {
			s.Bus.Publish(map[string]any{"type": "trace", "trace": sum})
		}
	}
}

func (s *Server) postSpans(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	var items []json.RawMessage
	ifAbsent := false
	if err := json.Unmarshal(body, &items); err != nil {
		var wrapped struct {
			Spans    []json.RawMessage `json:"spans"`
			IfAbsent bool              `json:"if_absent"` // log-derived spans: never overwrite a service's own
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil || wrapped.Spans == nil {
			http.Error(w, `send a JSON list of spans, or {"spans": [...], "if_absent": false}`, http.StatusBadRequest)
			return
		}
		items, ifAbsent = wrapped.Spans, wrapped.IfAbsent
	}
	if len(items) > maxBatch {
		http.Error(w, fmt.Sprintf("at most %d spans per request", maxBatch), http.StatusRequestEntityTooLarge)
		return
	}
	spans := make([]model.Span, 0, len(items))
	rejected := 0
	reasons := []string{} // why, once each, the first few: enough for a sender to fix itself
	for _, it := range items {
		sp, err := model.Clean(it)
		if err != nil {
			rejected++
			if msg := err.Error(); len(reasons) < 5 && !contains(reasons, msg) {
				reasons = append(reasons, msg)
			}
			continue
		}
		spans = append(spans, sp)
	}
	res, err := s.Store.AddReport(spans, ifAbsent)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.spansReceived.Add(int64(len(spans) - res.Dropped))
	s.spansRejected.Add(int64(rejected))
	s.Publish(res.Touched)
	if res.Touched == nil {
		res.Touched = []string{}
	}
	out := map[string]any{"accepted": len(spans) - res.Dropped, "rejected": rejected, "traces": res.Touched}
	if rejected > 0 {
		out["errors"] = reasons
	}
	if res.Dropped > 0 {
		out["dropped"] = res.Dropped
		out["note"] = s.capNote(res)
	}
	writeJSON(w, out)
}

func (s *Server) capNote(res store.AddResult) string {
	return fmt.Sprintf("%d spans were not stored: a trace holds at most %d spans (max_spans_per_trace) and %s full",
		res.Dropped, s.Store.MaxSpansPerTrace, quoteSome(res.Full))
}

func quoteSome(ids []string) string {
	const show = 3
	q := make([]string, 0, show)
	for _, id := range ids[:min(show, len(ids))] {
		q = append(q, strconv.Quote(id))
	}
	switch {
	case len(ids) > show:
		return fmt.Sprintf("%s and %d more are", strings.Join(q, ", "), len(ids)-show)
	case len(ids) == 1:
		return q[0] + " is"
	}
	return strings.Join(q, ", ") + " are"
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// fail answers a store error without its text (SQL and file paths stay in the server's log). A request
// whose caller has gone gets nothing: its query was cancelled with it.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	http.Error(w, "the store failed; the reason is in the server's log", http.StatusInternalServerError)
}

// postOTLP takes OpenTelemetry traces in OTLP/JSON: what any OpenTelemetry SDK's OTLP HTTP exporter sends
// with the JSON encoding to <server>/v1/traces. The reply is the protocol's ExportTraceServiceResponse.
func (s *Server) postOTLP(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-protobuf") {
		http.Error(w, "OTLP over protobuf is not supported yet; set the exporter's encoding to JSON (OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/json)", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	spans, skipped, err := otlp.Decode(body)
	if err != nil {
		http.Error(w, "not OTLP/JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	res, err := s.Store.AddReport(spans, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.spansReceived.Add(int64(len(spans) - res.Dropped))
	s.spansRejected.Add(int64(skipped))
	s.Publish(res.Touched)
	out := map[string]any{}
	if skipped+res.Dropped > 0 {
		msg := []string{}
		if skipped > 0 {
			msg = append(msg, "spans without a trace id or a start time, with an id over 64 bytes or with a time that cannot be right were skipped")
		}
		if res.Dropped > 0 {
			msg = append(msg, s.capNote(res))
		}
		out["partialSuccess"] = map[string]any{"rejectedSpans": skipped + res.Dropped, "errorMessage": strings.Join(msg, "; ")}
	}
	writeJSON(w, out)
}

func (s *Server) getTraces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{Since: optInt(q, "since"), Until: optInt(q, "until"), MinMS: optInt(q, "min_ms"),
		Model: q.Get("model"), Service: q.Get("service"), Status: q.Get("status"), Client: q.Get("client"), Q: q.Get("q"), Kind: q.Get("kind"), Sort: q.Get("sort")}
	for _, k := range []string{"model", "service", "status", "client", "q", "kind"} {
		if len(q.Get(k)) > maxParam {
			http.Error(w, fmt.Sprintf("%s is too long: at most %d bytes", k, maxParam), http.StatusBadRequest)
			return
		}
	}
	if f.Sort != "" && f.Sort != "duration" && f.Sort != "start" {
		http.Error(w, `sort is "start" (newest first, the default) or "duration" (slowest first)`, http.StatusBadRequest)
		return
	}
	if v := optInt(q, "limit"); v != nil {
		f.Limit = int(min(max(*v, 0), 5000))
	}
	if v := optInt(q, "offset"); v != nil {
		f.Offset = int(min(max(*v, 0), 1<<30))
	}
	rows, err := s.Store.Traces(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"traces": rows}
	if q.Get("count") == "1" { // how many match in all: a second scan of the window, so only on request
		total, err := s.Store.CountTraces(r.Context(), f)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out["total"] = total
	}
	writeJSON(w, out)
}

func (s *Server) getTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit := 0 // every span, unless max_spans asks for the first few
	if v := optInt(r.URL.Query(), "max_spans"); v != nil {
		limit = int(min(max(*v, 0), 1<<30))
	}
	spans, err := s.Store.Trace(r.Context(), id, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(spans) == 0 {
		http.Error(w, "no such trace", http.StatusNotFound)
		return
	}
	sum, _ := s.Store.Summary(r.Context(), id)
	writeJSON(w, map[string]any{"trace": sum, "spans": spans})
}

func window(q map[string][]string, defaultMS int64) (int64, int64) {
	until := model.NowMS()
	if v, err := strconv.ParseInt(first(q["until"]), 10, 64); err == nil {
		until = v
	}
	since := until - defaultMS
	if v, err := strconv.ParseInt(first(q["since"]), 10, 64); err == nil {
		since = v
	}
	return since, until
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	since, until := window(r.URL.Query(), 3_600_000)
	bucket := int64(60_000)
	if v := optInt(r.URL.Query(), "bucket_ms"); v != nil {
		bucket = *v
	}
	st, err := s.Store.Stats(r.Context(), since, until, bucket)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) getFlow(w http.ResponseWriter, r *http.Request) {
	since, until := window(r.URL.Query(), 3_600_000)
	edges, err := s.Store.Edges(r.Context(), since, until)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	services, err := s.Store.Services(r.Context(), since, until)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"since_ms": since, "until_ms": until, "edges": edges, "services": services})
}

func (s *Server) getLoads(w http.ResponseWriter, r *http.Request) {
	since, until := window(r.URL.Query(), 3_600_000)
	loads, err := s.Store.Loads(r.Context(), since, until)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"since_ms": since, "until_ms": until, "loads": loads})
}

func (s *Server) getFacets(w http.ResponseWriter, r *http.Request) {
	since, until := window(r.URL.Query(), 86_400_000)
	f, err := s.Store.Facets(r.Context(), since, until)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, f)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	ch := s.Bus.Subscribe()
	defer s.Bus.Unsubscribe(ch)
	fmt.Fprint(w, ": hello\n\n")
	flusher.Flush()
	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case ev := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", ev)
			flusher.Flush()
		case <-keep.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "now_ms": model.NowMS(), "db": s.Store.Path,
		"uptime_s": int64(time.Since(s.started).Seconds())})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.serveUI(w, r, "index.html")
}

func (s *Server) uiFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if strings.Contains(name, "..") || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	s.serveUI(w, r, name)
}

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request, name string) {
	data, err := ui.Files.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "text/html; charset=utf-8"
	switch {
	case strings.HasSuffix(name, ".js"):
		ct = "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		ct = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write: %v", err)
	}
}

func optInt(q map[string][]string, key string) *int64 {
	v, err := strconv.ParseInt(first(q[key]), 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
