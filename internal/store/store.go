// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package store keeps everything in one SQLite file: WAL for readers beside the writer, synchronous FULL
// so a power cut loses no span that was acknowledged (the setting a database would use).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // the driver, pure Go

	"github.com/YauhenBichel/llm-hops/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS spans (
  trace_id  TEXT NOT NULL,
  span_id   TEXT NOT NULL,
  parent_id TEXT,
  service   TEXT NOT NULL,
  name      TEXT NOT NULL,
  start_ms  INTEGER NOT NULL,
  end_ms    INTEGER NOT NULL,
  status    TEXT NOT NULL,
  attrs     TEXT NOT NULL,
  PRIMARY KEY (trace_id, span_id)
);
CREATE INDEX IF NOT EXISTS spans_start ON spans(start_ms);
CREATE TABLE IF NOT EXISTS traces (
  trace_id TEXT PRIMARY KEY,
  start_ms INTEGER NOT NULL,
  end_ms   INTEGER NOT NULL,
  service  TEXT NOT NULL,
  name     TEXT NOT NULL,
  status   TEXT NOT NULL,
  spans    INTEGER NOT NULL,
  attrs    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS traces_start ON traces(start_ms);
`

// Store is safe for concurrent use; writes are serialised.
type Store struct {
	Path string
	db   *sql.DB
	mu   sync.Mutex
}

// Open creates the file and the schema when needed.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one connection: SQLite serialises anyway, and this keeps the pragmas on it
	for _, p := range []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL", "PRAGMA busy_timeout = 5000"} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if _, err := db.Exec(schema + rollupSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{Path: path, db: db}, nil
}

// Close closes the file.
func (s *Store) Close() error { return s.db.Close() }

// Add inserts spans (a repeat of the same span replaces it) and refreshes the summaries of the traces
// they touch. It returns the trace ids touched, sorted, for the live stream.
func (s *Store) Add(spans []model.Span) ([]string, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	set := map[string]bool{}
	for _, sp := range spans {
		set[sp.TraceID] = true
	}
	touched := make([]string, 0, len(set))
	for id := range set {
		touched = append(touched, id)
	}
	sort.Strings(touched)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	stmt, err := tx.Prepare("INSERT OR REPLACE INTO spans VALUES (?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, sp := range spans {
		attrs, _ := json.Marshal(sp.Attrs)
		var parent any
		if sp.ParentID != "" {
			parent = sp.ParentID
		}
		if _, err := stmt.Exec(sp.TraceID, sp.SpanID, parent, sp.Service, sp.Name, sp.StartMS, sp.EndMS, sp.Status, string(attrs)); err != nil {
			return nil, err
		}
	}
	for _, id := range touched {
		if err := refreshSummary(tx, id); err != nil {
			return nil, err
		}
	}
	return touched, tx.Commit()
}

type spanRow struct {
	id, parent, service, name string
	start, end                int64
	status, attrs             string
}

func refreshSummary(tx *sql.Tx, traceID string) error {
	rows, err := tx.Query("SELECT span_id, COALESCE(parent_id,''), service, name, start_ms, end_ms, status, attrs FROM spans WHERE trace_id=?", traceID)
	if err != nil {
		return err
	}
	var all []spanRow
	for rows.Next() {
		var r spanRow
		if err := rows.Scan(&r.id, &r.parent, &r.service, &r.name, &r.start, &r.end, &r.status, &r.attrs); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if len(all) == 0 {
		return nil
	}
	ids := map[string]bool{}
	for _, r := range all {
		ids[r.id] = true
	}
	var root *spanRow
	start, end := all[0].start, all[0].end
	status := "ok"
	for i := range all {
		r := &all[i]
		if r.start < start {
			start = r.start
		}
		if r.end > end {
			end = r.end
		}
		if r.status == "error" {
			status = "error"
		}
		isRoot := r.parent == "" || !ids[r.parent]
		if isRoot && (root == nil || r.start < root.start) {
			root = r
		}
	}
	if root == nil {
		root = &all[0]
	}
	rootRow := *root // a copy: the sort below moves the rows the pointer came from
	// the root's attributes first; what it lacks, the first later span that knows it supplies (a router in
	// front of a gateway knows the client, the gateway knows the wire, the queue time and the tokens)
	var rootAttrs model.Attrs
	_ = json.Unmarshal([]byte(rootRow.attrs), &rootAttrs)
	attrs := model.Attrs{}
	for _, k := range model.SummaryAttrs {
		if v, ok := rootAttrs[k]; ok && v != nil && v != "" {
			attrs[k] = v
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].start < all[j].start })
	for _, r := range all {
		if r.id == rootRow.id {
			continue
		}
		var a model.Attrs
		_ = json.Unmarshal([]byte(r.attrs), &a)
		for _, k := range model.SummaryAttrs {
			if _, have := attrs[k]; have {
				continue
			}
			if v, ok := a[k]; ok && v != nil && v != "" {
				attrs[k] = v
			}
		}
	}
	attrsJSON, _ := json.Marshal(attrs)
	_, err = tx.Exec("INSERT OR REPLACE INTO traces VALUES (?,?,?,?,?,?,?,?)",
		traceID, start, end, rootRow.service, rootRow.name, status, len(all), string(attrsJSON))
	return err
}

// Prune drops traces that started more than keepMS before nowMS and returns how many.
func (s *Store) Prune(keepMS, nowMS int64) (int64, error) {
	cutoff := nowMS - keepMS
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	if err := s.db.QueryRow("SELECT count(*) FROM traces WHERE start_ms < ?", cutoff).Scan(&n); err != nil {
		return 0, err
	}
	if _, err := s.db.Exec("DELETE FROM spans WHERE trace_id IN (SELECT trace_id FROM traces WHERE start_ms < ?)", cutoff); err != nil {
		return 0, err
	}
	_, err := s.db.Exec("DELETE FROM traces WHERE start_ms < ?", cutoff)
	return n, err
}

// Filter narrows the trace list.
type Filter struct {
	Since, Until      *int64
	Limit             int
	Model, Service    string
	Status, Client, Q string
	MinMS             *int64
}

// Traces lists trace summaries, newest first.
func (s *Store) Traces(f Filter) ([]model.TraceSummary, error) {
	where := []string{"1=1"}
	var args []any
	if f.Since != nil {
		where = append(where, "start_ms >= ?")
		args = append(args, *f.Since)
	}
	if f.Until != nil {
		where = append(where, "start_ms < ?")
		args = append(args, *f.Until)
	}
	if f.Service != "" {
		where = append(where, "service = ?")
		args = append(args, f.Service)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.MinMS != nil {
		where = append(where, "end_ms - start_ms >= ?")
		args = append(args, *f.MinMS)
	}
	if f.Model != "" {
		where = append(where, "json_extract(attrs, '$.model') = ?")
		args = append(args, f.Model)
	}
	if f.Client != "" {
		where = append(where, "json_extract(attrs, '$.client') = ?")
		args = append(args, f.Client)
	}
	if f.Q != "" {
		where = append(where, "(attrs LIKE ? OR name LIKE ? OR trace_id LIKE ?)")
		like := "%" + f.Q + "%"
		args = append(args, like, like, like)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 5000 {
		limit = 5000
	}
	args = append(args, limit)
	rows, err := s.db.Query("SELECT trace_id,start_ms,end_ms,service,name,status,spans,attrs FROM traces WHERE "+
		strings.Join(where, " AND ")+" ORDER BY start_ms DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.TraceSummary{}
	for rows.Next() {
		t, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanSummary(r scanner) (model.TraceSummary, error) {
	var t model.TraceSummary
	var attrs string
	if err := r.Scan(&t.TraceID, &t.StartMS, &t.EndMS, &t.Service, &t.Name, &t.Status, &t.Spans, &attrs); err != nil {
		return t, err
	}
	t.DurationMS = max(0, t.EndMS-t.StartMS)
	t.Attrs = model.Attrs{}
	_ = json.Unmarshal([]byte(attrs), &t.Attrs)
	return t, nil
}

// Summary returns one trace's row, or nil.
func (s *Store) Summary(traceID string) (*model.TraceSummary, error) {
	t, err := scanSummary(s.db.QueryRow("SELECT trace_id,start_ms,end_ms,service,name,status,spans,attrs FROM traces WHERE trace_id=?", traceID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Trace returns a trace's spans, by start time.
func (s *Store) Trace(traceID string) ([]model.Span, error) {
	rows, err := s.db.Query("SELECT trace_id,span_id,COALESCE(parent_id,''),service,name,start_ms,end_ms,status,attrs "+
		"FROM spans WHERE trace_id=? ORDER BY start_ms, end_ms DESC", traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Span{}
	for rows.Next() {
		var sp model.Span
		var attrs string
		if err := rows.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentID, &sp.Service, &sp.Name, &sp.StartMS, &sp.EndMS, &sp.Status, &attrs); err != nil {
			return nil, err
		}
		sp.Attrs = model.Attrs{}
		_ = json.Unmarshal([]byte(attrs), &sp.Attrs)
		out = append(out, sp)
	}
	return out, rows.Err()
}

// HopStat is a hop's count and percentiles.
type HopStat struct {
	N     int   `json:"n"`
	P50MS int64 `json:"p50_ms"`
	P95MS int64 `json:"p95_ms"`
}

// ModelStat is one model's row in the statistics.
type ModelStat struct {
	Model     string `json:"model"`
	Requests  int    `json:"requests"`
	Errors    int    `json:"errors"`
	P50MS     int64  `json:"p50_ms"`
	P95MS     int64  `json:"p95_ms"`
	TokensOut int64  `json:"tokens_out"`
	ms        []int64
}

// Bucket is requests per time bucket.
type Bucket struct {
	T int64 `json:"t"`
	N int   `json:"n"`
}

// Stats is what a window adds up to.
type Stats struct {
	SinceMS       int64              `json:"since_ms"`
	UntilMS       int64              `json:"until_ms"`
	BucketMS      int64              `json:"bucket_ms"`
	Requests      int                `json:"requests"`
	Errors        int                `json:"errors"`
	P50MS         int64              `json:"p50_ms"`
	P95MS         int64              `json:"p95_ms"`
	MaxMS         int64              `json:"max_ms"`
	ModelSwitches int                `json:"model_switches"`
	Approx        bool               `json:"approx"` // part of the window comes from daily rollups (percentiles weighted)
	Hops          map[string]HopStat `json:"hops"`
	Buckets       []Bucket           `json:"buckets"`
	Models        []ModelStat        `json:"models"`
}

// Stats computes counts, percentiles per hop, requests per bucket and per-model rows for a window.
func (s *Store) Stats(sinceMS, untilMS, bucketMS int64) (Stats, error) {
	if bucketMS < 1000 {
		bucketMS = 1000
	}
	st := Stats{SinceMS: sinceMS, UntilMS: untilMS, BucketMS: bucketMS, Hops: map[string]HopStat{}, Buckets: []Bucket{}, Models: []ModelStat{}}
	rows, err := s.db.Query("SELECT start_ms, end_ms, status, attrs FROM traces WHERE start_ms >= ? AND start_ms < ? ORDER BY start_ms", sinceMS, untilMS)
	if err != nil {
		return st, err
	}
	var durations []int64
	perModel := map[string]*ModelStat{}
	buckets := map[int64]int{}
	prevModel := ""
	for rows.Next() {
		var start, end int64
		var status, attrsJSON string
		if err := rows.Scan(&start, &end, &status, &attrsJSON); err != nil {
			rows.Close()
			return st, err
		}
		var attrs model.Attrs
		_ = json.Unmarshal([]byte(attrsJSON), &attrs)
		d := max(0, end-start)
		durations = append(durations, d)
		st.Requests++
		if status == "error" {
			st.Errors++
		}
		m := attrString(attrs, "model")
		if m == "" {
			m = "?"
		}
		row := perModel[m]
		if row == nil {
			row = &ModelStat{Model: m}
			perModel[m] = row
		}
		row.Requests++
		if status == "error" {
			row.Errors++
		}
		row.ms = append(row.ms, d)
		if v, ok := attrs["completion_tokens"].(float64); ok {
			row.TokensOut += int64(v)
		}
		b := (start / bucketMS) * bucketMS
		buckets[b]++
		backend := attrString(attrs, "backend")
		if (backend == "" || backend == "ollama") && m != "?" {
			if prevModel != "" && m != prevModel {
				st.ModelSwitches++
			}
			prevModel = m
		}
	}
	rows.Close()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	st.P50MS, st.P95MS = pct(durations, 0.5), pct(durations, 0.95)
	if len(durations) > 0 {
		st.MaxMS = durations[len(durations)-1]
	}
	// "where the time goes" counts leaf hops only: a span with children is the sum of them, and a nested
	// "request" is another service's root, not a hop
	hops, err := s.db.Query("SELECT s.name, s.end_ms - s.start_ms FROM spans s JOIN traces t ON t.trace_id = s.trace_id "+
		"WHERE t.start_ms >= ? AND t.start_ms < ? AND s.parent_id IS NOT NULL AND s.name != 'request' "+
		"AND NOT EXISTS (SELECT 1 FROM spans c WHERE c.trace_id = s.trace_id AND c.parent_id = s.span_id)", sinceMS, untilMS)
	if err != nil {
		return st, err
	}
	perHop := map[string][]int64{}
	for hops.Next() {
		var name string
		var d int64
		if err := hops.Scan(&name, &d); err != nil {
			hops.Close()
			return st, err
		}
		perHop[name] = append(perHop[name], max(0, d))
	}
	hops.Close()
	for name, v := range perHop {
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		st.Hops[name] = HopStat{N: len(v), P50MS: pct(v, 0.5), P95MS: pct(v, 0.95)}
	}
	for b, n := range buckets {
		st.Buckets = append(st.Buckets, Bucket{T: b, N: n})
	}
	sort.Slice(st.Buckets, func(i, j int) bool { return st.Buckets[i].T < st.Buckets[j].T })
	for _, r := range perModel {
		sort.Slice(r.ms, func(i, j int) bool { return r.ms[i] < r.ms[j] })
		r.P50MS, r.P95MS = pct(r.ms, 0.5), pct(r.ms, 0.95)
		st.Models = append(st.Models, *r)
	}
	sort.Slice(st.Models, func(i, j int) bool { return st.Models[i].Requests > st.Models[j].Requests })
	if err := s.mergeDaily(&st, sinceMS, untilMS); err != nil {
		return st, err
	}
	return st, nil
}

// Edge is one parent→child hop of the flow map, aggregated.
type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	N     int    `json:"n"`
	AvgMS int64  `json:"avg_ms"`
}

// Edges aggregates the parent→child hops of a window by service and name.
func (s *Store) Edges(sinceMS, untilMS int64) ([]Edge, error) {
	rows, err := s.db.Query("SELECT p.service, p.name, s.service, s.name, count(*), avg(s.end_ms - s.start_ms) "+
		"FROM spans s JOIN spans p ON p.trace_id = s.trace_id AND p.span_id = s.parent_id "+
		"JOIN traces t ON t.trace_id = s.trace_id WHERE t.start_ms >= ? AND t.start_ms < ? GROUP BY 1,2,3,4", sinceMS, untilMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Edge{}
	for rows.Next() {
		var ps, pn, cs, cn string
		var n int
		var avg sql.NullFloat64
		if err := rows.Scan(&ps, &pn, &cs, &cn, &n, &avg); err != nil {
			return nil, err
		}
		out = append(out, Edge{From: ps + ":" + pn, To: cs + ":" + cn, N: n, AvgMS: int64(avg.Float64 + 0.5)})
	}
	return out, rows.Err()
}

// ServiceStat is one service's presence in a window.
type ServiceStat struct {
	Service string `json:"service"`
	Traces  int    `json:"traces"`
	Errors  int    `json:"errors"`
}

// Services lists the services seen in a window.
func (s *Store) Services(sinceMS, untilMS int64) ([]ServiceStat, error) {
	rows, err := s.db.Query("SELECT s.service, count(DISTINCT s.trace_id), COALESCE(sum(s.status='error'),0) FROM spans s "+
		"JOIN traces t ON t.trace_id = s.trace_id WHERE t.start_ms >= ? AND t.start_ms < ? GROUP BY 1 ORDER BY 1", sinceMS, untilMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceStat{}
	for rows.Next() {
		var st ServiceStat
		if err := rows.Scan(&st.Service, &st.Traces, &st.Errors); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Facets lists the distinct values the filters can take in a window.
func (s *Store) Facets(sinceMS, untilMS int64) (map[string][]string, error) {
	out := map[string][]string{}
	for _, key := range []string{"model", "client", "wire", "role"} {
		rows, err := s.db.Query(fmt.Sprintf("SELECT DISTINCT json_extract(attrs, '$.%s') FROM traces WHERE start_ms >= ? AND start_ms < ? "+
			"AND json_extract(attrs, '$.%s') IS NOT NULL ORDER BY 1", key, key), sinceMS, untilMS)
		if err != nil {
			return nil, err
		}
		vals := []string{}
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			vals = append(vals, fmt.Sprintf("%v", v))
		}
		rows.Close()
		out[key] = vals
	}
	rows, err := s.db.Query("SELECT DISTINCT service FROM traces WHERE start_ms >= ? AND start_ms < ? ORDER BY 1", sinceMS, untilMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	svcs := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		svcs = append(svcs, v)
	}
	out["service"] = svcs
	return out, nil
}

func attrString(a model.Attrs, k string) string {
	if v, ok := a[k]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func pct(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	k := int(p*float64(len(sorted)-1) + 0.5)
	if k < 0 {
		k = 0
	}
	if k > len(sorted)-1 {
		k = len(sorted) - 1
	}
	return sorted[k]
}
