// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// What the API answers, reduced to what the tools print. Kept separate from the server's types so the MCP
// side depends only on the HTTP contract.

type statsOut struct {
	SinceMS       int64 `json:"since_ms"`
	UntilMS       int64 `json:"until_ms"`
	Requests      int   `json:"requests"`
	Errors        int   `json:"errors"`
	P50MS         int64 `json:"p50_ms"`
	P95MS         int64 `json:"p95_ms"`
	MaxMS         int64 `json:"max_ms"`
	ModelSwitches int   `json:"model_switches"`
	Approx        bool  `json:"approx"`
	Hops          map[string]struct {
		N     int   `json:"n"`
		P50MS int64 `json:"p50_ms"`
		P95MS int64 `json:"p95_ms"`
	} `json:"hops"`
	Models []struct {
		Model     string `json:"model"`
		Requests  int    `json:"requests"`
		Errors    int    `json:"errors"`
		P50MS     int64  `json:"p50_ms"`
		P95MS     int64  `json:"p95_ms"`
		TokensOut int64  `json:"tokens_out"`
	} `json:"models"`
	Backends []struct {
		Backend          string `json:"backend"`
		Provider         string `json:"provider"`
		Requests         int    `json:"requests"`
		Errors           int    `json:"errors"`
		PromptTokens     int64  `json:"prompt_tokens"`
		CompletionTokens int64  `json:"completion_tokens"`
		P50MS            int64  `json:"p50_ms"`
		P95MS            int64  `json:"p95_ms"`
	} `json:"backends"`
}

func (st statsOut) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Window: %d requests, %d errors, p50 %s, p95 %s, max %s, %d model switches%s.\n",
		st.Requests, st.Errors, ms(st.P50MS), ms(st.P95MS), ms(st.MaxMS), st.ModelSwitches, approx(st.Approx))
	if len(st.Backends) > 0 {
		b.WriteString("Where they went:\n")
		for _, be := range st.Backends {
			fmt.Fprintf(&b, "  %-6s %-10s %5d requests, %d errors, tokens in %d / out %d, p50 %s, p95 %s\n",
				be.Backend, be.Provider, be.Requests, be.Errors, be.PromptTokens, be.CompletionTokens, ms(be.P50MS), ms(be.P95MS))
		}
	}
	if len(st.Hops) > 0 {
		b.WriteString("Time per hop (leaf hops, median / p95):\n")
		names := make([]string, 0, len(st.Hops))
		for k := range st.Hops {
			names = append(names, k)
		}
		sort.Slice(names, func(i, j int) bool {
			return st.Hops[names[i]].P50MS*int64(st.Hops[names[i]].N) > st.Hops[names[j]].P50MS*int64(st.Hops[names[j]].N)
		})
		for _, k := range names {
			h := st.Hops[k]
			fmt.Fprintf(&b, "  %-10s n=%-5d %s / %s\n", k, h.N, ms(h.P50MS), ms(h.P95MS))
		}
	}
	if len(st.Models) > 0 {
		b.WriteString("Per model:\n")
		for _, m := range st.Models {
			fmt.Fprintf(&b, "  %-28s %5d requests, %d errors, p50 %s, p95 %s, %d tokens out\n", m.Model, m.Requests, m.Errors, ms(m.P50MS), ms(m.P95MS), m.TokensOut)
		}
	}
	return b.String()
}

type flowOut struct {
	Services []struct {
		Service string `json:"service"`
		Traces  int    `json:"traces"`
		Errors  int    `json:"errors"`
	} `json:"services"`
	Edges []struct {
		From  string `json:"from"`
		To    string `json:"to"`
		N     int    `json:"n"`
		AvgMS int64  `json:"avg_ms"`
	} `json:"edges"`
}

func (f flowOut) text() string {
	var b strings.Builder
	b.WriteString("Services:\n")
	for _, s := range f.Services {
		fmt.Fprintf(&b, "  %-12s %d requests, %d errors\n", s.Service, s.Traces, s.Errors)
	}
	b.WriteString("Hops (parent -> child: count, average):\n")
	for _, e := range f.Edges {
		fmt.Fprintf(&b, "  %s -> %s: %d, %s\n", e.From, e.To, e.N, ms(e.AvgMS))
	}
	return b.String()
}

type traceSummary struct {
	TraceID    string         `json:"trace_id"`
	StartMS    int64          `json:"start_ms"`
	DurationMS int64          `json:"duration_ms"`
	Service    string         `json:"service"`
	Status     string         `json:"status"`
	Spans      int            `json:"spans"`
	Attrs      map[string]any `json:"attrs"`
}

type tracesOut struct {
	Traces []traceSummary `json:"traces"`
	Total  *int           `json:"total"` // how many match in all; absent from a server that does not count
}

func (t *tracesOut) sortByDuration() {
	sort.SliceStable(t.Traces, func(i, j int) bool { return t.Traces[i].DurationMS > t.Traces[j].DurationMS })
}

// text prints the rows, then where this page sits among the matches and how to get the next one.
func (t tracesOut) text(offset int, slowest bool) string {
	if len(t.Traces) == 0 {
		if offset > 0 && t.Total != nil && *t.Total > 0 {
			return fmt.Sprintf("No more requests: %d match, and offset is %d.", *t.Total, offset)
		}
		return "No requests match."
	}
	var b strings.Builder
	for _, tr := range t.Traces {
		fmt.Fprintf(&b, "%s  %-8s %-10s %-26s %-5s %8s", when(tr.StartMS), tr.Service, cutText(s(tr.Attrs["client"]), 40), cutText(s(tr.Attrs["model"]), 60), tr.Status, ms(tr.DurationMS))
		if v, ok := tr.Attrs["queue_ms"].(float64); ok && v > 0 {
			fmt.Fprintf(&b, "  queue %s", ms(int64(v)))
		}
		if v, ok := tr.Attrs["ttft_ms"].(float64); ok && v > 0 {
			fmt.Fprintf(&b, "  first token %s", ms(int64(v)))
		}
		if v, ok := tr.Attrs["error"].(string); ok && v != "" {
			fmt.Fprintf(&b, "  error=%s", cutText(v, maxAttrValue))
		}
		fmt.Fprintf(&b, "  id=%s\n", tr.TraceID)
	}
	if t.Total != nil {
		order := "newest first"
		if slowest {
			order = "slowest first"
		}
		last := offset + len(t.Traces)
		fmt.Fprintf(&b, "Showing %d-%d of %d, %s.", offset+1, last, *t.Total, order)
		if more := *t.Total - last; more > 0 {
			if slowest {
				fmt.Fprintf(&b, " %d more match: raise limit (at most %d).", more, maxTraces)
			} else {
				fmt.Fprintf(&b, " %d more match: call again with offset=%d.", more, last)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

type traceOut struct {
	Trace *traceSummary `json:"trace"`
	Spans []struct {
		SpanID   string         `json:"span_id"`
		ParentID string         `json:"parent_id"`
		Service  string         `json:"service"`
		Name     string         `json:"name"`
		StartMS  int64          `json:"start_ms"`
		EndMS    int64          `json:"end_ms"`
		Status   string         `json:"status"`
		Attrs    map[string]any `json:"attrs"`
	} `json:"spans"`
}

// What one span's attributes may take in the waterfall: a value is cut, and past the budget the rest are
// counted, not printed.
const (
	maxAttrValue = 200
	maxAttrText  = 1000
	maxDepth     = 20
)

// text prints the waterfall: at most maxSpans spans, with a note when the trace has more.
func (t traceOut) text(maxSpans int) string {
	if len(t.Spans) == 0 {
		return "No such trace."
	}
	total := len(t.Spans)
	if t.Trace != nil && t.Trace.Spans > total {
		total = t.Trace.Spans
	}
	if maxSpans > 0 && len(t.Spans) > maxSpans { // a server that does not cut for us
		t.Spans = t.Spans[:maxSpans]
	}
	var b strings.Builder
	if t.Trace != nil {
		fmt.Fprintf(&b, "Trace %s: %s, %s, %d spans, status %s\n", t.Trace.TraceID, when(t.Trace.StartMS), ms(t.Trace.DurationMS), t.Trace.Spans, t.Trace.Status)
	}
	t0 := t.Spans[0].StartMS
	parent := make(map[string]string, len(t.Spans))
	for _, sp := range t.Spans {
		if sp.StartMS < t0 {
			t0 = sp.StartMS
		}
		parent[sp.SpanID] = sp.ParentID
	}
	// a span's depth is its parent's plus one; the memo makes a long chain one step a span, and the cap
	// keeps the indent readable and ends a cycle
	depth := make(map[string]int, len(t.Spans))
	var depthOf func(id string, guard int) int
	depthOf = func(id string, guard int) int {
		if d, ok := depth[id]; ok {
			return d
		}
		d := 0
		if p, ok := parent[id]; ok && p != "" && guard < maxDepth {
			if _, known := parent[p]; known {
				d = min(depthOf(p, guard+1)+1, maxDepth)
			}
		}
		depth[id] = d
		return d
	}
	// parents first (the spans come by start time), so the recursion stays shallow
	for _, sp := range t.Spans {
		depthOf(sp.SpanID, 0)
	}
	for _, sp := range t.Spans {
		fmt.Fprintf(&b, "%s%-10s %-10s +%-8s %-8s %s", strings.Repeat("  ", depth[sp.SpanID]), sp.Service, sp.Name, ms(sp.StartMS-t0), ms(sp.EndMS-sp.StartMS), sp.Status)
		keys := make([]string, 0, len(sp.Attrs))
		for k := range sp.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		used := 0
		for i, k := range keys {
			kv := fmt.Sprintf(" %s=%s", k, cutText(fmt.Sprintf("%v", sp.Attrs[k]), maxAttrValue))
			if used+len(kv) > maxAttrText {
				fmt.Fprintf(&b, " (+%d more attributes)", len(keys)-i)
				break
			}
			used += len(kv)
			b.WriteString(kv)
		}
		b.WriteByte('\n')
	}
	if more := total - len(t.Spans); more > 0 {
		fmt.Fprintf(&b, "Truncated: the first %d of %d spans by start time, %d more not shown", len(t.Spans), total, more)
		if len(t.Spans) < maxSpansCap {
			fmt.Fprintf(&b, " (max_spans goes up to %d)", maxSpansCap)
		}
		b.WriteString(".\n")
	}
	return b.String()
}

// cutText shortens a value for printing, on a character boundary, and marks the cut.
func cutText(v string, n int) string {
	if len(v) <= n {
		return v
	}
	for n > 0 && !utf8.RuneStart(v[n]) {
		n--
	}
	return v[:n] + "…"
}

func ms(v int64) string {
	switch {
	case v < 1000:
		return fmt.Sprintf("%d ms", v)
	case v < 60_000:
		return fmt.Sprintf("%.1f s", float64(v)/1000)
	default:
		return fmt.Sprintf("%.1f min", float64(v)/60_000)
	}
}

func when(msSinceEpoch int64) string {
	return time.UnixMilli(msSinceEpoch).UTC().Format("01-02 15:04:05")
}

func s(v any) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%v", v)
}

func approx(a bool) string {
	if a {
		return " (part of the window from daily rollups, percentiles approximate)"
	}
	return ""
}
