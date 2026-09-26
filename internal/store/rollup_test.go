// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/demo"
	"github.com/YauhenBichel/llm-hops/internal/model"
)

func TestRollupKeepsTheCountsAfterTheSpansAreGone(t *testing.T) {
	s := open(t)
	day := 24 * int64(time.Hour/time.Millisecond)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC).UnixMilli()
	// 40 traces twenty days ago, 30 traces two days ago, 10 traces today
	for _, w := range []struct {
		ago int64
		n   int
	}{{20, 40}, {2, 30}, {0, 10}} {
		g := demo.New(w.ago, now-w.ago*day, w.n, 600)
		for i := 0; i < w.n; i++ {
			if _, err := s.Add(g.Next()); err != nil {
				t.Fatal(err)
			}
		}
	}
	before, _ := s.Stats(now-30*day, now+day, day)
	if before.Requests != 80 || before.Approx {
		t.Fatalf("before: %+v", before)
	}
	n, err := s.RollupAndPrune(14*day, now)
	if err != nil || n != 40 {
		t.Fatalf("rolled %d %v", n, err)
	}
	left, _ := s.Traces(Filter{})
	if len(left) != 40 {
		t.Fatalf("%d traces left, want the 40 recent ones", len(left))
	}
	daily, _ := s.Daily(now-30*day, now)
	var sum int
	for _, d := range daily {
		sum += d.Requests
		if d.Day != dayOf(now-20*day) {
			t.Fatalf("rolled-up day %s, want %s", d.Day, dayOf(now-20*day))
		}
	}
	if sum != 40 {
		t.Fatalf("daily rows add up to %d", sum)
	}
	after, _ := s.Stats(now-30*day, now+day, day)
	if after.Requests != 80 || !after.Approx || after.Errors != before.Errors {
		t.Fatalf("after: requests %d approx %v errors %d/%d", after.Requests, after.Approx, after.Errors, before.Errors)
	}
	if after.P50MS == 0 || after.P95MS < after.P50MS {
		t.Fatalf("percentiles: %+v", after)
	}
	var tokBefore, tokAfter int64
	for _, m := range before.Models {
		tokBefore += m.TokensOut
	}
	for _, m := range after.Models {
		tokAfter += m.TokensOut
	}
	if tokBefore != tokAfter {
		t.Fatalf("tokens %d != %d", tokBefore, tokAfter)
	}
	// a window inside the retained days is exact and unmarked
	recent, _ := s.Stats(now-3*day, now+day, day)
	if recent.Requests != 40 || recent.Approx {
		t.Fatalf("recent: %+v", recent)
	}
	// a second run rolls nothing more and changes nothing
	n, _ = s.RollupAndPrune(14*day, now)
	again, _ := s.Stats(now-30*day, now+day, day)
	if n != 0 || again.Requests != 80 {
		t.Fatalf("second run: %d %+v", n, again)
	}
}

func TestExportRoundTrip(t *testing.T) {
	s := open(t)
	g := demo.New(5, 1_000_000, 20, 200)
	var want int
	for i := 0; i < 20; i++ {
		sp := g.Next()
		want += len(sp)
		s.Add(sp)
	}
	var got []model.Span
	if err := s.ExportSpans(0, 1<<62, func(sp model.Span) error { got = append(got, sp); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != want {
		t.Fatalf("exported %d of %d spans", len(got), want)
	}
	s2 := open(t)
	if _, err := s2.Add(got); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Stats(0, 1<<62, 60_000)
	b, _ := s2.Stats(0, 1<<62, 60_000)
	if a.Requests != b.Requests || a.P95MS != b.P95MS || len(a.Hops) != len(b.Hops) {
		t.Fatalf("the rebuilt store differs: %+v vs %+v", a, b)
	}
}
