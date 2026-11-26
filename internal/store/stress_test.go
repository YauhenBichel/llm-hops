// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/model"
)

// What a stress test found: one huge trace, readers against the writer, a file that only grows, a damaged
// file. Each test fails on the code before the fix.

// schemaV024 is the schema as v0.2.4 made it: no index on the parent, no auto_vacuum.
const schemaV024 = `
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

// oldFile writes a database the way v0.2.4 did, with one trace of a root and two hops and one daily row.
func oldFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL", schemaV024,
		`INSERT INTO spans VALUES ('old','r',NULL,'gateway','request',1000,1900,'ok','{"model":"m"}')`,
		`INSERT INTO spans VALUES ('old','q','r','gateway','queue',1000,1100,'ok','{}')`,
		`INSERT INTO spans VALUES ('old','u','r','ollama','upstream',1100,1900,'ok','{}')`,
		`INSERT INTO traces VALUES ('old',1000,1900,'gateway','request','ok',3,'{"model":"m"}')`,
		`INSERT INTO daily VALUES ('1969-12-01','m',7,1,10,20,99)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasParentIndex(t *testing.T, s *Store) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='spans_parent'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestAnOldFileIsMigratedAndKeepsItsData(t *testing.T) {
	path := oldFile(t)
	for round := 1; round <= 2; round++ { // the second start must find nothing left to do
		s, err := Open(path)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if !hasParentIndex(t, s) {
			t.Fatalf("round %d: the index spans(trace_id, parent_id) is missing", round)
		}
		ctx := context.Background()
		spans, err := s.Trace(ctx, "old", 0)
		if err != nil || len(spans) != 3 {
			t.Fatalf("round %d: the old trace: %d spans, %v", round, len(spans), err)
		}
		st, err := s.Stats(ctx, 0, 10_000, 60_000)
		if err != nil || st.Requests != round || st.Hops["queue"].N != 1 || st.Hops["upstream"].P50MS != 800 {
			t.Fatalf("round %d: stats over the old rows: %+v %v", round, st, err)
		}
		if daily, _ := s.Daily(ctx, -90*86_400_000, 0); len(daily) != 1 || daily[0].Requests != 7 {
			t.Fatalf("round %d: the old daily row: %+v", round, daily)
		}
		if s.Shrinks() {
			t.Fatal("an old file must keep its auto_vacuum mode until `llm-hops vacuum`: changing it needs a full rebuild")
		}
		// and it takes new spans beside the old ones
		if _, err := s.Add([]model.Span{{TraceID: "new", SpanID: "r", Service: "gateway", Name: "request", StartMS: 2000, EndMS: 2500, Status: "ok", Attrs: model.Attrs{}}}); err != nil {
			t.Fatal(err)
		}
		if rows, _ := s.Traces(ctx, Filter{}); len(rows) != 2 {
			t.Fatalf("round %d: %d traces", round, len(rows))
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLeafHopsUseTheParentIndex(t *testing.T) {
	s := open(t)
	for name, q := range map[string]string{"leaf hops": leafHopsSQL} {
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+q, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail + "\n")
		}
		rows.Close()
		if !strings.Contains(plan.String(), "spans_parent") {
			t.Fatalf("%s do not use the index, so they are quadratic in spans per trace:\n%s", name, plan.String())
		}
	}
}

// chain is one trace of n spans, each the child of the one before: the shape that made /stats quadratic.
func chain(id string, n int, startMS int64) []model.Span {
	out := make([]model.Span, n)
	for i := range out {
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("s%d", i-1)
		}
		out[i] = model.Span{TraceID: id, SpanID: fmt.Sprintf("s%d", i), ParentID: parent, Service: "svc", Name: "hop",
			StartMS: startMS + int64(i), EndMS: startMS + int64(i) + 3, Status: "ok", Attrs: model.Attrs{}}
	}
	return out
}

func TestStatsAndFlowStayFastOnOneHugeTrace(t *testing.T) {
	s := open(t)
	if _, err := s.Add(chain("big", 20_000, 1000)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t0 := time.Now()
	st, err := s.Stats(ctx, 0, 1<<40, 60_000)
	if err != nil || st.Hops["hop"].N != 1 { // of a chain only the last span is a leaf
		t.Fatalf("stats: %+v %v", st.Hops, err)
	}
	edges, err := s.Edges(ctx, 0, 1<<40)
	if err != nil || len(edges) != 1 || edges[0].N != 19_999 {
		t.Fatalf("edges: %+v %v", edges, err)
	}
	// without the index this is 20 000 x 20 000 row visits: minutes; with it, well under a second
	if d := time.Since(t0); d > 20*time.Second {
		t.Fatalf("stats and flow over one 20 000-span trace took %s", d)
	}
}

func TestACancelledRequestStopsItsQuery(t *testing.T) {
	s := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Stats(ctx, 0, 1<<40, 60_000); !errors.Is(err, context.Canceled) {
		t.Fatalf("stats with a cancelled context: %v", err)
	}
	if _, err := s.Edges(ctx, 0, 1<<40); !errors.Is(err, context.Canceled) {
		t.Fatalf("edges with a cancelled context: %v", err)
	}
	if _, err := s.Traces(ctx, Filter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("traces with a cancelled context: %v", err)
	}

	// a query already running stops too: make it slow the way it was (no index, one long trace)
	if _, err := s.Add(chain("big", 20_000, 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP INDEX spans_parent"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := s.Stats(ctx, 0, 1<<40, 60_000)
	if err == nil {
		t.Skip("the machine answered the slow query inside 100 ms; nothing to cancel")
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("the query ran %s after its request was gone", d)
	}
	// and the connection is usable afterwards
	if _, err := s.Traces(context.Background(), Filter{}); err != nil {
		t.Fatal(err)
	}
}

func TestATraceIsCappedAndSaysSo(t *testing.T) {
	s := open(t)
	s.MaxSpansPerTrace = 10
	res, err := s.AddReport(chain("t", 8, 1000), false)
	if err != nil || res.Dropped != 0 || len(res.Touched) != 1 {
		t.Fatalf("under the cap: %+v %v", res, err)
	}
	// 13 spans: 8 are repeats (stored again), 2 fit, 3 do not
	more := chain("t", 13, 1000)
	more[0].EndMS = 5000
	other := model.Span{TraceID: "u", SpanID: "r", Service: "a", Name: "request", StartMS: 1, EndMS: 2, Status: "ok", Attrs: model.Attrs{}}
	res, err = s.AddReport(append(more, other), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Dropped != 3 || len(res.Full) != 1 || res.Full[0] != "t" || len(res.Touched) != 2 {
		t.Fatalf("at the cap: %+v", res)
	}
	ctx := context.Background()
	sum, _ := s.Summary(ctx, "t")
	if sum.Spans != 10 || sum.EndMS != 5000 {
		t.Fatalf("the trace holds %d spans (want 10), and a repeat of a stored span must still replace it (end %d)", sum.Spans, sum.EndMS)
	}
	// a batch of nothing but spans over the cap writes nothing and touches nothing
	res, err = s.AddReport(chain("t", 13, 1000)[10:], true)
	if err != nil || res.Dropped != 3 || len(res.Touched) != 0 {
		t.Fatalf("all over the cap: %+v %v", res, err)
	}
	if s.Dropped() != 6 {
		t.Fatalf("dropped counter: %d", s.Dropped())
	}
	s.MaxSpansPerTrace = 0 // no cap
	if res, _ := s.AddReport(chain("t", 13, 1000), false); res.Dropped != 0 {
		t.Fatalf("without a cap: %+v", res)
	}
}

func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish in %s", what, d)
	}
}

func TestReadersAndTheWriterDoNotWaitForEachOther(t *testing.T) {
	s := open(t)
	if _, err := s.Add(chain("t", 5, 1000)); err != nil {
		t.Fatal(err)
	}
	// a write in progress (its transaction open on the writer's connection) ...
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO spans VALUES ('w','r',NULL,'a','request',1,2,'ok','{}')"); err != nil {
		t.Fatal(err)
	}
	// ... does not hold up a reader, which sees what was committed before it
	within(t, 3*time.Second, "a read beside an open write", func() {
		rows, err := s.Traces(context.Background(), Filter{})
		if err != nil || len(rows) != 1 {
			t.Errorf("read beside a write: %d rows, %v", len(rows), err)
		}
		if _, err := s.Stats(context.Background(), 0, 1<<40, 60_000); err != nil {
			t.Error(err)
		}
	})
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// a slow read (its rows still open) does not hold up the writer
	rows, err := s.rdb.Query("SELECT span_id FROM spans")
	if err != nil || !rows.Next() {
		t.Fatal(err)
	}
	defer rows.Close()
	within(t, 3*time.Second, "a write beside an open read", func() {
		if _, err := s.Add(chain("t2", 5, 2000)); err != nil {
			t.Error(err)
		}
	})
}

func TestTheReadersCannotWrite(t *testing.T) {
	s := open(t)
	if _, err := s.rdb.Exec("DELETE FROM traces"); err == nil {
		t.Fatal("a read connection accepted a write")
	}
}

func TestTracesBySlowestFirstWithPagesAndACount(t *testing.T) {
	s := open(t)
	var spans []model.Span
	for i := 0; i < 50; i++ { // trace i starts at i seconds and lasts (i*37 mod 50) ms: order by start != order by duration
		spans = append(spans, model.Span{TraceID: fmt.Sprintf("t%02d", i), SpanID: "r", Service: "gateway", Name: "request",
			StartMS: int64(i) * 1000, EndMS: int64(i)*1000 + int64(i*37%50), Status: "ok", Attrs: model.Attrs{"model": "m"}})
	}
	if _, err := s.Add(spans); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	top, err := s.Traces(ctx, Filter{Sort: "duration", Limit: 5})
	if err != nil || len(top) != 5 {
		t.Fatal(top, err)
	}
	for i, want := range []int64{49, 48, 47, 46, 45} {
		if top[i].DurationMS != want {
			t.Fatalf("slowest first, row %d: %d ms, want %d", i, top[i].DurationMS, want)
		}
	}
	next, _ := s.Traces(ctx, Filter{Sort: "duration", Limit: 5, Offset: 5})
	if len(next) != 5 || next[0].DurationMS != 44 {
		t.Fatalf("the second page: %+v", next)
	}
	newest, _ := s.Traces(ctx, Filter{Limit: 3, Offset: 3})
	if len(newest) != 3 || newest[0].TraceID != "t46" {
		t.Fatalf("newest first from the fourth: %+v", newest)
	}
	min := int64(40)
	if n, err := s.CountTraces(ctx, Filter{MinMS: &min, Limit: 2}); err != nil || n != 10 {
		t.Fatalf("count of the slow ones: %d %v", n, err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// days adds perDay traces a day for the given days before now, each with a long attribute.
func days(t *testing.T, s *Store, now int64, perDay int, daysAgo ...int) {
	t.Helper()
	const day = 86_400_000
	for _, d := range daysAgo {
		var spans []model.Span
		for i := 0; i < perDay; i++ {
			start := dayStart(now) - int64(d)*day + int64(i)*1000
			spans = append(spans, model.Span{TraceID: fmt.Sprintf("d%d-%d", d, i), SpanID: "r", Service: "gateway", Name: "request",
				StartMS: start, EndMS: start + 50, Status: "ok", Attrs: model.Attrs{"model": "m", "error": strings.Repeat("x", 500)}})
		}
		if _, err := s.Add(spans); err != nil {
			t.Fatal(err)
		}
	}
}

func TestANewFileShrinksAfterAPrune(t *testing.T) {
	s := open(t)
	if !s.Shrinks() {
		t.Fatal("a new file must be made with auto_vacuum INCREMENTAL")
	}
	now := model.NowMS()
	days(t, s, now, 2000, 30, 29, 0)
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	before := fileSize(t, s.Path)
	if n, err := s.RollupAndPrune(14*86_400_000, now); err != nil || n != 4000 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	freed, err := s.FreePages()
	if err != nil || freed == 0 {
		t.Fatalf("freed %d bytes, %v", freed, err)
	}
	after := fileSize(t, s.Path)
	if after > before/2 {
		t.Fatalf("two thirds of the data went, the file went from %d to %d bytes", before, after)
	}
	if rows, _ := s.Traces(context.Background(), Filter{Limit: 5000}); len(rows) != 2000 {
		t.Fatalf("today's traces after the shrink: %d", len(rows))
	}
}

func TestVacuumMakesAnOldFileShrink(t *testing.T) {
	path := oldFile(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Shrinks() {
		t.Fatal("the old file already shrinks")
	}
	if freed, err := s.FreePages(); err != nil || freed != 0 {
		t.Fatalf("FreePages on a file that cannot shrink: %d %v", freed, err)
	}
	s.Close()
	if _, _, err := Vacuum(path); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Shrinks() {
		t.Fatal("after vacuum the file still does not shrink")
	}
	if spans, _ := s.Trace(context.Background(), "old", 0); len(spans) != 3 || !hasParentIndex(t, s) {
		t.Fatalf("after vacuum: %d spans", len(spans))
	}
	if _, _, err := Vacuum(filepath.Join(t.TempDir(), "absent.db")); err == nil {
		t.Fatal("vacuum of a file that is not there must fail, not create one")
	}
}

func TestPruneToSizeDropsWholeOldDaysAndNeverToday(t *testing.T) {
	s := open(t)
	now := model.NowMS()
	days(t, s, now, 500, 3, 2, 1, 0)
	ctx := context.Background()
	full, err := s.SizeBytes(ctx)
	if err != nil || full == 0 {
		t.Fatal(full, err)
	}
	// room for about half: the two oldest days go, as daily rows
	n, over, err := s.PruneToSize(full*6/10, now)
	if err != nil || over || n != 1000 {
		t.Fatalf("pruned %d, over %v, %v", n, over, err)
	}
	if size, _ := s.SizeBytes(ctx); size > full*6/10 {
		t.Fatalf("still %d bytes of %d", size, full)
	}
	if daily, _ := s.Daily(ctx, now-10*86_400_000, now); len(daily) != 2 || daily[0].Requests != 500 {
		t.Fatalf("the pruned days must survive as daily rows: %+v", daily)
	}
	// a limit below today's data: yesterday goes, today stays, and the caller is told
	n, over, err = s.PruneToSize(1, now)
	if err != nil || !over || n != 500 {
		t.Fatalf("pruned %d, over %v, %v", n, over, err)
	}
	if rows, _ := s.Traces(ctx, Filter{Limit: 5000}); len(rows) != 500 {
		t.Fatalf("today's traces: %d", len(rows))
	}
}

func TestADamagedFileIsRefusedWithAReason(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.db")
	s, err := Open(good)
	if err != nil {
		t.Fatal(err)
	}
	days(t, s, model.NowMS(), 2000, 0)
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	data, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"cut in half":    data[:len(data)/2/4096*4096],
		"cut mid-page":   data[:len(data)/3+17],
		"not a database": []byte(strings.Repeat("this is not a database\n", 400)),
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".db")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Open(path)
		if err == nil {
			s.Close()
			t.Fatalf("%s: opened without a word", name)
		}
		if !strings.Contains(err.Error(), "not a usable llm-hops database") || !strings.Contains(err.Error(), path) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// the good one still opens, and an empty file is a new database
	if s, err = Open(good); err != nil {
		t.Fatal(err)
	}
	s.Close()
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(empty); err != nil {
		t.Fatal(err)
	}
	s.Close()
}
