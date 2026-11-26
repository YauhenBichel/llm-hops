// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package store keeps everything in one SQLite file: WAL for readers beside the writer, synchronous FULL
// so a power cut loses no span that was acknowledged (the setting a database would use).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

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
CREATE INDEX IF NOT EXISTS spans_parent ON spans(trace_id, parent_id);
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

// readers is how many connections serve queries beside the one that writes.
const readers = 4

// Store is safe for concurrent use. Writes are serialised on one connection; queries run on a small pool
// of read-only connections beside it (WAL lets readers work while the writer commits), so a burst of page
// loads does not make the senders wait.
type Store struct {
	Path string
	// MaxSpansPerTrace caps a trace: once it holds this many spans, new ones are dropped and counted
	// (a repeat of a stored span still replaces it). Zero means no cap.
	MaxSpansPerTrace int

	db      *sql.DB // the writer, one connection
	rdb     *sql.DB // the readers; the same handle as db for an in-memory database
	mu      sync.Mutex
	dropped atomic.Int64
}

// Open creates the file and the schema when needed, and brings an older file up to date: the only change
// so far is the index spans(trace_id, parent_id), built once on the first start (seconds per million spans).
// A file that fails SQLite's quick check is refused, with the reason.
func Open(path string) (*Store, error) {
	fresh := true
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		fresh = false
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer: SQLite has one anyway, and this keeps the pragmas on it
	pragmas := []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL", "PRAGMA busy_timeout = 5000"}
	if fresh {
		// a new file gives pages back after a prune (FreePages); the mode can only be chosen before the
		// first table exists, so an older file keeps its own until `llm-hops vacuum` rebuilds it
		pragmas = append([]string{"PRAGMA auto_vacuum = INCREMENTAL"}, pragmas...)
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			if !fresh {
				return nil, damaged(path, err.Error())
			}
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if !fresh {
		var res string
		if err := db.QueryRow("PRAGMA quick_check(1)").Scan(&res); err != nil {
			db.Close()
			return nil, damaged(path, err.Error())
		} else if res != "ok" {
			db.Close()
			return nil, damaged(path, res)
		}
		var have int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='spans_parent'").Scan(&have); err == nil && have == 0 {
			var tables int
			_ = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='spans'").Scan(&tables)
			if tables > 0 {
				log.Printf("%s: building the index spans(trace_id, parent_id) once; this takes seconds per million spans", path)
			}
		}
	}
	if _, err := db.Exec(schema + rollupSchema); err != nil {
		db.Close()
		if !fresh {
			return nil, damaged(path, err.Error())
		}
		return nil, err
	}
	s := &Store{Path: path, db: db, rdb: db}
	if path != ":memory:" && !strings.Contains(path, "mode=memory") {
		rdb, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
		if err != nil {
			db.Close()
			return nil, err
		}
		rdb.SetMaxOpenConns(readers)
		rdb.SetMaxIdleConns(readers)
		if err := rdb.Ping(); err != nil {
			rdb.Close()
			db.Close()
			return nil, err
		}
		s.rdb = rdb
	}
	return s, nil
}

func damaged(path, why string) error {
	return fmt.Errorf("%s is not a usable llm-hops database (%s): restore a copy, or move the file away to start empty", path, why)
}

// Close closes the file.
func (s *Store) Close() error {
	var err error
	if s.rdb != s.db {
		err = s.rdb.Close()
	}
	return errors.Join(err, s.db.Close())
}

// Dropped is how many spans were not stored since the process started because their trace was at
// MaxSpansPerTrace.
func (s *Store) Dropped() int64 { return s.dropped.Load() }

// AddResult is what an insert did.
type AddResult struct {
	Touched []string // the trace ids written to, sorted, for the live stream
	Dropped int      // spans not stored: their trace is at MaxSpansPerTrace
	Full    []string // the traces that were at the cap, sorted
}

// Add inserts spans (a repeat of the same trace_id and span_id replaces the stored span) and refreshes the
// summaries of the traces they touch. It returns the trace ids touched, sorted, for the live stream.
func (s *Store) Add(spans []model.Span) ([]string, error) {
	r, err := s.add(spans, true)
	return r.Touched, err
}

// AddIfAbsent inserts spans that are not there yet and leaves existing ones alone: for spans derived from
// a log when the service may also post its own, exact ones, which must win whichever arrives first.
func (s *Store) AddIfAbsent(spans []model.Span) ([]string, error) {
	r, err := s.add(spans, false)
	return r.Touched, err
}

// AddReport is Add or AddIfAbsent for a caller that tells its sender what was dropped at the cap.
func (s *Store) AddReport(spans []model.Span, ifAbsent bool) (AddResult, error) {
	return s.add(spans, !ifAbsent)
}

func (s *Store) add(spans []model.Span, replace bool) (AddResult, error) {
	var res AddResult
	if len(spans) == 0 {
		return res, nil
	}
	perTrace := map[string]int{}
	for _, sp := range spans {
		perTrace[sp.TraceID]++
	}
	ids := make([]string, 0, len(perTrace))
	for id := range perTrace {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	// room holds, for the traces this batch could push over the cap, how many new spans still fit; the
	// others (nearly all) skip the per-span check
	room := map[string]int{}
	if s.MaxSpansPerTrace > 0 {
		for _, id := range ids {
			var have int
			if err := tx.QueryRow("SELECT count(*) FROM spans WHERE trace_id=?", id).Scan(&have); err != nil {
				return res, err
			}
			if have+perTrace[id] > s.MaxSpansPerTrace {
				room[id] = max(0, s.MaxSpansPerTrace-have)
			}
		}
	}
	verb := "REPLACE"
	if !replace {
		verb = "IGNORE"
	}
	stmt, err := tx.Prepare("INSERT OR " + verb + " INTO spans VALUES (?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return res, err
	}
	defer stmt.Close()
	written, full := map[string]bool{}, map[string]bool{}
	for _, sp := range spans {
		if left, capped := room[sp.TraceID]; capped {
			var one int
			err := tx.QueryRow("SELECT 1 FROM spans WHERE trace_id=? AND span_id=?", sp.TraceID, sp.SpanID).Scan(&one)
			if err == sql.ErrNoRows {
				if left == 0 {
					res.Dropped++
					full[sp.TraceID] = true
					continue
				}
				room[sp.TraceID] = left - 1
			} else if err != nil {
				return res, err
			}
		}
		attrs, _ := json.Marshal(sp.Attrs)
		var parent any
		if sp.ParentID != "" {
			parent = sp.ParentID
		}
		if _, err := stmt.Exec(sp.TraceID, sp.SpanID, parent, sp.Service, sp.Name, sp.StartMS, sp.EndMS, sp.Status, string(attrs)); err != nil {
			return res, err
		}
		written[sp.TraceID] = true
	}
	for _, id := range ids {
		if full[id] {
			res.Full = append(res.Full, id)
		}
		if !written[id] {
			continue
		}
		res.Touched = append(res.Touched, id)
		if err := refreshSummary(tx, id); err != nil {
			return AddResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AddResult{}, err
	}
	s.dropped.Add(int64(res.Dropped))
	return res, nil
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
	Limit, Offset     int
	Model, Service    string
	Status, Client, Q string
	MinMS             *int64
	Kind              string // "" = requests only (no loads or unloads); "load"; "any"
	Sort              string // "" = newest first; "duration" = slowest first
}

func (f Filter) where() (string, []any) {
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
	switch f.Kind {
	case "", "request":
		where = append(where, "json_extract(attrs, '$.kind') IS NULL")
	case "any":
	default:
		where = append(where, "json_extract(attrs, '$.kind') = ?")
		args = append(args, f.Kind)
	}
	if f.Q != "" {
		where = append(where, "(attrs LIKE ? OR name LIKE ? OR trace_id LIKE ?)")
		like := "%" + f.Q + "%"
		args = append(args, like, like, like)
	}
	return strings.Join(where, " AND "), args
}

// Traces lists trace summaries: newest first, or slowest first with Sort "duration" (the whole window is
// ranked, not a page of it). Offset skips rows, for the next page.
func (s *Store) Traces(ctx context.Context, f Filter) ([]model.TraceSummary, error) {
	where, args := f.where()
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 5000 {
		limit = 5000
	}
	order := "start_ms DESC"
	if f.Sort == "duration" {
		order = "end_ms - start_ms DESC, start_ms DESC"
	}
	args = append(args, limit, max(0, f.Offset))
	rows, err := s.rdb.QueryContext(ctx, "SELECT trace_id,start_ms,end_ms,service,name,status,spans,attrs FROM traces WHERE "+
		where+" ORDER BY "+order+" LIMIT ? OFFSET ?", args...)
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

// CountTraces is how many traces the filter matches, whatever its Limit and Offset.
func (s *Store) CountTraces(ctx context.Context, f Filter) (int, error) {
	where, args := f.where()
	var n int
	err := s.rdb.QueryRowContext(ctx, "SELECT count(*) FROM traces WHERE "+where, args...).Scan(&n)
	return n, err
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
func (s *Store) Summary(ctx context.Context, traceID string) (*model.TraceSummary, error) {
	t, err := scanSummary(s.rdb.QueryRowContext(ctx, "SELECT trace_id,start_ms,end_ms,service,name,status,spans,attrs FROM traces WHERE trace_id=?", traceID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Written returns the row of a trace that was just written, read on the writer's own connection: the
// answer to a sender must not wait in the readers' queue.
func (s *Store) Written(traceID string) (*model.TraceSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := scanSummary(s.db.QueryRow("SELECT trace_id,start_ms,end_ms,service,name,status,spans,attrs FROM traces WHERE trace_id=?", traceID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Trace returns a trace's spans, by start time: all of them, or the first limit when limit is positive.
func (s *Store) Trace(ctx context.Context, traceID string, limit int) ([]model.Span, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := s.rdb.QueryContext(ctx, "SELECT trace_id,span_id,COALESCE(parent_id,''),service,name,start_ms,end_ms,status,attrs "+
		"FROM spans WHERE trace_id=? ORDER BY start_ms, end_ms DESC LIMIT ?", traceID, limit)
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

// BackendStat is where the requests went: local or cloud, and which provider and model.
type BackendStat struct {
	Backend          string `json:"backend"`  // local | cloud, or the value the sender used
	Provider         string `json:"provider"` // ollama, anthropic, openai, ...
	Requests         int    `json:"requests"`
	Errors           int    `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	P50MS            int64  `json:"p50_ms"`
	P95MS            int64  `json:"p95_ms"`
	ms               []int64
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
	Loads         int                `json:"loads"`   // model loads seen in Ollama's log (traces of kind load)
	LoadMS        int64              `json:"load_ms"` // time spent loading, in the window
	Approx        bool               `json:"approx"`  // part of the window comes from daily rollups (percentiles weighted)
	Backends      []BackendStat      `json:"backends"`
	Hops          map[string]HopStat `json:"hops"`
	Buckets       []Bucket           `json:"buckets"`
	Models        []ModelStat        `json:"models"`
}

// "where the time goes" counts leaf hops only: a span with children is the sum of them, and a nested
// "request" is another service's root, not a hop. The NOT EXISTS is answered by the index
// spans(trace_id, parent_id); without it every span scans its whole trace (quadratic in spans per trace).
const leafHopsSQL = "SELECT s.name, s.end_ms - s.start_ms FROM spans s JOIN traces t ON t.trace_id = s.trace_id " +
	"WHERE t.start_ms >= ? AND t.start_ms < ? AND s.parent_id IS NOT NULL AND s.name != 'request' " +
	"AND NOT EXISTS (SELECT 1 FROM spans c WHERE c.trace_id = s.trace_id AND c.parent_id = s.span_id)"

// Stats computes counts, percentiles per hop, requests per bucket and per-model rows for a window.
func (s *Store) Stats(ctx context.Context, sinceMS, untilMS, bucketMS int64) (Stats, error) {
	if bucketMS < 1000 {
		bucketMS = 1000
	}
	st := Stats{SinceMS: sinceMS, UntilMS: untilMS, BucketMS: bucketMS, Hops: map[string]HopStat{}, Buckets: []Bucket{}, Models: []ModelStat{}, Backends: []BackendStat{}}
	perBackend := map[string]*BackendStat{}
	rows, err := s.rdb.QueryContext(ctx, "SELECT start_ms, end_ms, status, attrs FROM traces WHERE start_ms >= ? AND start_ms < ? ORDER BY start_ms", sinceMS, untilMS)
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
		switch attrString(attrs, "kind") {
		case "load": // a model load from Ollama's log: not a request
			st.Loads++
			st.LoadMS += d
			continue
		case "unload":
			continue
		}
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
		// where it went: a router says local or cloud; a gateway line is local by definition
		be := attrString(attrs, "backend")
		provider := attrString(attrs, "provider")
		switch be {
		case "local", "cloud":
		case "":
			be = "local"
		default: // a gateway's backend name (ollama, cpu, audio ...) is the provider of a local request
			if provider == "" {
				provider = be
			}
			be = "local"
		}
		if provider == "" {
			provider = "?"
		}
		bk := be + "/" + provider
		bs := perBackend[bk]
		if bs == nil {
			bs = &BackendStat{Backend: be, Provider: provider}
			perBackend[bk] = bs
		}
		bs.Requests++
		if status == "error" {
			bs.Errors++
		}
		bs.ms = append(bs.ms, d)
		if v, ok := attrs["prompt_tokens"].(float64); ok {
			bs.PromptTokens += int64(v)
		}
		if v, ok := attrs["completion_tokens"].(float64); ok {
			bs.CompletionTokens += int64(v)
		}
		backend := attrString(attrs, "backend")
		if (backend == "" || backend == "ollama") && m != "?" {
			if prevModel != "" && m != prevModel {
				st.ModelSwitches++
			}
			prevModel = m
		}
	}
	if err := rows.Err(); err != nil { // a cancelled request ends the loop early: not a short answer
		rows.Close()
		return st, err
	}
	rows.Close()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	st.P50MS, st.P95MS = pct(durations, 0.5), pct(durations, 0.95)
	if len(durations) > 0 {
		st.MaxMS = durations[len(durations)-1]
	}
	hops, err := s.rdb.QueryContext(ctx, leafHopsSQL, sinceMS, untilMS)
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
	if err := hops.Err(); err != nil {
		hops.Close()
		return st, err
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
	for _, b := range perBackend {
		sort.Slice(b.ms, func(i, j int) bool { return b.ms[i] < b.ms[j] })
		b.P50MS, b.P95MS = pct(b.ms, 0.5), pct(b.ms, 0.95)
		st.Backends = append(st.Backends, *b)
	}
	sort.Slice(st.Backends, func(i, j int) bool {
		if st.Backends[i].Backend != st.Backends[j].Backend {
			return st.Backends[i].Backend < st.Backends[j].Backend // cloud before local
		}
		return st.Backends[i].Requests > st.Backends[j].Requests
	})
	if err := s.mergeDaily(ctx, &st, sinceMS, untilMS); err != nil {
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
func (s *Store) Edges(ctx context.Context, sinceMS, untilMS int64) ([]Edge, error) {
	rows, err := s.rdb.QueryContext(ctx, "SELECT p.service, p.name, s.service, s.name, count(*), avg(s.end_ms - s.start_ms) "+
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
func (s *Store) Services(ctx context.Context, sinceMS, untilMS int64) ([]ServiceStat, error) {
	rows, err := s.rdb.QueryContext(ctx, "SELECT s.service, count(DISTINCT s.trace_id), COALESCE(sum(s.status='error'),0) FROM spans s "+
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
func (s *Store) Facets(ctx context.Context, sinceMS, untilMS int64) (map[string][]string, error) {
	out := map[string][]string{}
	for _, key := range []string{"model", "client", "wire", "role"} {
		rows, err := s.rdb.QueryContext(ctx, fmt.Sprintf("SELECT DISTINCT json_extract(attrs, '$.%s') FROM traces WHERE start_ms >= ? AND start_ms < ? "+
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
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		out[key] = vals
	}
	rows, err := s.rdb.QueryContext(ctx, "SELECT DISTINCT service FROM traces WHERE start_ms >= ? AND start_ms < ? ORDER BY 1", sinceMS, untilMS)
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
	return out, rows.Err()
}

// Load is one model load from Ollama's journal, as the timeline draws it.
type Load struct {
	Model   string `json:"model"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

// Loads lists the model loads in a window, oldest first, plus the last one before it (what was loaded
// when the window opened).
func (s *Store) Loads(ctx context.Context, sinceMS, untilMS int64) ([]Load, error) {
	rows, err := s.rdb.QueryContext(ctx, "SELECT start_ms, end_ms, json_extract(attrs, '$.model') FROM traces "+
		"WHERE json_extract(attrs, '$.kind') = 'load' AND start_ms < ? AND (start_ms >= ? OR start_ms = "+
		"(SELECT max(start_ms) FROM traces WHERE json_extract(attrs, '$.kind') = 'load' AND start_ms < ?)) ORDER BY start_ms",
		untilMS, sinceMS, sinceMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Load{}
	for rows.Next() {
		var l Load
		var m sql.NullString
		if err := rows.Scan(&l.StartMS, &l.EndMS, &m); err != nil {
			return nil, err
		}
		l.Model = m.String
		if l.Model == "" {
			l.Model = "?"
		}
		out = append(out, l)
	}
	return out, rows.Err()
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
