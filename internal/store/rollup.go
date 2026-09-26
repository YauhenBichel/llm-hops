// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/model"
)

// Spans are kept for keep_days; what came before survives as one row per day and model in `daily`:
// requests, errors, tokens out, the day's median and 95th percentile. So the stats view can show 90 days
// on a 14-day store. A percentile over several rolled-up days is a request-weighted mean of the days'
// percentiles: close, and marked as such by the API (`approx: true`).
const rollupSchema = `
CREATE TABLE IF NOT EXISTS daily (
  day        TEXT NOT NULL,
  model      TEXT NOT NULL,
  requests   INTEGER NOT NULL,
  errors     INTEGER NOT NULL,
  p50_ms     INTEGER NOT NULL,
  p95_ms     INTEGER NOT NULL,
  tokens_out INTEGER NOT NULL,
  PRIMARY KEY (day, model)
);
`

// DailyRow is one day of one model.
type DailyRow struct {
	Day       string `json:"day"`
	Model     string `json:"model"`
	Requests  int    `json:"requests"`
	Errors    int    `json:"errors"`
	P50MS     int64  `json:"p50_ms"`
	P95MS     int64  `json:"p95_ms"`
	TokensOut int64  `json:"tokens_out"`
}

func dayOf(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }

// RollupAndPrune folds the traces that started before cutoff into `daily` (replacing the rows of those days,
// so a re-run is safe), then drops their spans. Only whole days before the cutoff's day are rolled up: the
// cutoff's own day stays as spans until the next run, so a day is never half in each table.
func (s *Store) RollupAndPrune(keepMS, nowMS int64) (int64, error) {
	cutoff := nowMS - keepMS
	dayCut := time.UnixMilli(cutoff).UTC().Truncate(24 * time.Hour).UnixMilli() // whole days only
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query("SELECT start_ms, end_ms, status, attrs FROM traces WHERE start_ms < ? ORDER BY start_ms", dayCut)
	if err != nil {
		return 0, err
	}
	type acc struct {
		req, errs int
		ms        []int64
		tokens    int64
	}
	agg := map[[2]string]*acc{}
	var n int64
	for rows.Next() {
		var start, end int64
		var status, attrsJSON string
		if err := rows.Scan(&start, &end, &status, &attrsJSON); err != nil {
			rows.Close()
			return 0, err
		}
		n++
		var attrs model.Attrs
		_ = json.Unmarshal([]byte(attrsJSON), &attrs)
		m := attrString(attrs, "model")
		if m == "" {
			m = "?"
		}
		k := [2]string{dayOf(start), m}
		a := agg[k]
		if a == nil {
			a = &acc{}
			agg[k] = a
		}
		a.req++
		if status == "error" {
			a.errs++
		}
		a.ms = append(a.ms, max(0, end-start))
		if v, ok := attrs["completion_tokens"].(float64); ok {
			a.tokens += int64(v)
		}
	}
	rows.Close()
	if n == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	for k, a := range agg {
		sort.Slice(a.ms, func(i, j int) bool { return a.ms[i] < a.ms[j] })
		// a day may already have a row from an earlier partial run: fold the counts, keep the newer percentiles
		var oldReq, oldErr int
		var oldTok int64
		_ = tx.QueryRow("SELECT requests, errors, tokens_out FROM daily WHERE day=? AND model=?", k[0], k[1]).Scan(&oldReq, &oldErr, &oldTok)
		if _, err := tx.Exec("INSERT OR REPLACE INTO daily VALUES (?,?,?,?,?,?,?)",
			k[0], k[1], a.req+oldReq, a.errs+oldErr, pct(a.ms, 0.5), pct(a.ms, 0.95), a.tokens+oldTok); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec("DELETE FROM spans WHERE trace_id IN (SELECT trace_id FROM traces WHERE start_ms < ?)", dayCut); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM traces WHERE start_ms < ?", dayCut); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Daily lists the rolled-up rows whose day falls in the window.
func (s *Store) Daily(sinceMS, untilMS int64) ([]DailyRow, error) {
	rows, err := s.db.Query("SELECT day, model, requests, errors, p50_ms, p95_ms, tokens_out FROM daily WHERE day >= ? AND day <= ? ORDER BY day, model",
		dayOf(sinceMS), dayOf(untilMS))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DailyRow{}
	for rows.Next() {
		var r DailyRow
		if err := rows.Scan(&r.Day, &r.Model, &r.Requests, &r.Errors, &r.P50MS, &r.P95MS, &r.TokensOut); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OldestTraceMS is the start of the oldest trace still held as spans, or 0 when there is none.
func (s *Store) OldestTraceMS() int64 {
	var v int64
	_ = s.db.QueryRow("SELECT COALESCE(min(start_ms), 0) FROM traces").Scan(&v)
	return v
}

// mergeDaily adds the rolled-up days before the oldest span-held trace into st, marking it approximate.
func (s *Store) mergeDaily(st *Stats, sinceMS, untilMS int64) error {
	oldest := s.OldestTraceMS()
	limit := untilMS
	if oldest > 0 {
		limit = min(untilMS, oldest)
	}
	if sinceMS >= limit {
		return nil
	}
	daily, err := s.Daily(sinceMS, limit-1)
	if err != nil {
		return err
	}
	if len(daily) == 0 {
		return nil
	}
	// only days entirely before the oldest span-held day count; that day's spans are already in st
	oldestDay := ""
	if oldest > 0 {
		oldestDay = dayOf(oldest)
	}
	byModel := map[string]*ModelStat{}
	for i := range st.Models {
		byModel[st.Models[i].Model] = &st.Models[i]
	}
	var wP50, wP95, wN int64
	perDay := map[string]int{}
	for _, d := range daily {
		if oldestDay != "" && d.Day >= oldestDay {
			continue
		}
		st.Requests += d.Requests
		st.Errors += d.Errors
		wP50 += d.P50MS * int64(d.Requests)
		wP95 += d.P95MS * int64(d.Requests)
		wN += int64(d.Requests)
		if d.P95MS > st.MaxMS {
			st.MaxMS = d.P95MS
		}
		perDay[d.Day] += d.Requests
		m := byModel[d.Model]
		if m == nil {
			st.Models = append(st.Models, ModelStat{Model: d.Model})
			m = &st.Models[len(st.Models)-1]
			byModel[d.Model] = m
		}
		m.Requests += d.Requests
		m.Errors += d.Errors
		m.TokensOut += d.TokensOut
		if m.P95MS < d.P95MS {
			m.P95MS = d.P95MS
		}
		if m.P50MS == 0 {
			m.P50MS = d.P50MS
		}
	}
	if wN == 0 {
		return nil
	}
	// the window's percentiles: the span-held part and the rolled-up part, weighted by requests
	spanN := int64(st.Requests) - wN
	st.P50MS = (st.P50MS*spanN + wP50) / (spanN + wN)
	st.P95MS = (st.P95MS*spanN + wP95) / (spanN + wN)
	st.Approx = true
	for day, n := range perDay {
		t, _ := time.Parse("2006-01-02", day)
		st.Buckets = append(st.Buckets, Bucket{T: t.UnixMilli(), N: n})
	}
	sort.Slice(st.Buckets, func(i, j int) bool { return st.Buckets[i].T < st.Buckets[j].T })
	sort.Slice(st.Models, func(i, j int) bool { return st.Models[i].Requests > st.Models[j].Requests })
	return nil
}

// ExportSpans streams every span of the traces that started in the window, oldest first, one per call of fn.
func (s *Store) ExportSpans(sinceMS, untilMS int64, fn func(model.Span) error) error {
	rows, err := s.db.Query("SELECT s.trace_id,s.span_id,COALESCE(s.parent_id,''),s.service,s.name,s.start_ms,s.end_ms,s.status,s.attrs "+
		"FROM spans s JOIN traces t ON t.trace_id = s.trace_id WHERE t.start_ms >= ? AND t.start_ms < ? ORDER BY t.start_ms, s.start_ms", sinceMS, untilMS)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sp model.Span
		var attrs string
		if err := rows.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentID, &sp.Service, &sp.Name, &sp.StartMS, &sp.EndMS, &sp.Status, &attrs); err != nil {
			return err
		}
		sp.Attrs = model.Attrs{}
		if err := json.Unmarshal([]byte(attrs), &sp.Attrs); err != nil {
			return fmt.Errorf("span %s/%s: %w", sp.TraceID, sp.SpanID, err)
		}
		if err := fn(sp); err != nil {
			return err
		}
	}
	return rows.Err()
}
