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
}

// New wires the routes.
func New(st *store.Store, bus *Broadcast) *Server {
	s := &Server{Store: st, Bus: bus, Title: "llm-hops", started: time.Now(), mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /api/v1/spans", s.postSpans)
	s.mux.HandleFunc("GET /api/v1/traces", s.getTraces)
	s.mux.HandleFunc("GET /api/v1/traces/{id}", s.getTrace)
	s.mux.HandleFunc("GET /api/v1/stats", s.getStats)
	s.mux.HandleFunc("GET /api/v1/flow", s.getFlow)
	s.mux.HandleFunc("GET /api/v1/facets", s.getFacets)
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
	for _, id := range touched {
		if sum, err := s.Store.Summary(id); err == nil && sum != nil {
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
	if err := json.Unmarshal(body, &items); err != nil {
		var wrapped struct {
			Spans []json.RawMessage `json:"spans"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil || wrapped.Spans == nil {
			http.Error(w, `send a JSON list of spans, or {"spans": [...]}`, http.StatusBadRequest)
			return
		}
		items = wrapped.Spans
	}
	if len(items) > maxBatch {
		http.Error(w, fmt.Sprintf("at most %d spans per request", maxBatch), http.StatusRequestEntityTooLarge)
		return
	}
	spans := make([]model.Span, 0, len(items))
	rejected := 0
	for _, it := range items {
		sp, err := model.Clean(it)
		if err != nil {
			rejected++
			continue
		}
		spans = append(spans, sp)
	}
	touched, err := s.Store.Add(spans)
	if err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.spansReceived.Add(int64(len(spans)))
	s.spansRejected.Add(int64(rejected))
	s.Publish(touched)
	if touched == nil {
		touched = []string{}
	}
	writeJSON(w, map[string]any{"accepted": len(spans), "rejected": rejected, "traces": touched})
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
	touched, err := s.Store.Add(spans)
	if err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.spansReceived.Add(int64(len(spans)))
	s.spansRejected.Add(int64(skipped))
	s.Publish(touched)
	out := map[string]any{}
	if skipped > 0 {
		out["partialSuccess"] = map[string]any{"rejectedSpans": skipped, "errorMessage": "spans without a trace id or a start time were skipped"}
	}
	writeJSON(w, out)
}

func (s *Server) getTraces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{Since: optInt(q, "since"), Until: optInt(q, "until"), MinMS: optInt(q, "min_ms"),
		Model: q.Get("model"), Service: q.Get("service"), Status: q.Get("status"), Client: q.Get("client"), Q: q.Get("q")}
	if v := optInt(q, "limit"); v != nil {
		f.Limit = int(*v)
	}
	rows, err := s.Store.Traces(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"traces": rows})
}

func (s *Server) getTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spans, err := s.Store.Trace(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(spans) == 0 {
		http.Error(w, "no such trace", http.StatusNotFound)
		return
	}
	sum, _ := s.Store.Summary(id)
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
	st, err := s.Store.Stats(since, until, bucket)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, st)
}

func (s *Server) getFlow(w http.ResponseWriter, r *http.Request) {
	since, until := window(r.URL.Query(), 3_600_000)
	edges, err := s.Store.Edges(since, until)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	services, err := s.Store.Services(since, until)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"since_ms": since, "until_ms": until, "edges": edges, "services": services})
}

func (s *Server) getFacets(w http.ResponseWriter, r *http.Request) {
	since, until := window(r.URL.Query(), 86_400_000)
	f, err := s.Store.Facets(since, until)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
