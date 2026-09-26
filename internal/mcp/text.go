// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"fmt"
	"sort"
	"strings"
	"time"
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
}

func (t *tracesOut) sortByDuration() {
	sort.Slice(t.Traces, func(i, j int) bool { return t.Traces[i].DurationMS > t.Traces[j].DurationMS })
}

func (t tracesOut) text() string {
	if len(t.Traces) == 0 {
		return "No requests match."
	}
	var b strings.Builder
	for _, tr := range t.Traces {
		fmt.Fprintf(&b, "%s  %-8s %-10s %-26s %-5s %8s", when(tr.StartMS), tr.Service, s(tr.Attrs["client"]), s(tr.Attrs["model"]), tr.Status, ms(tr.DurationMS))
		if v, ok := tr.Attrs["queue_ms"].(float64); ok && v > 0 {
			fmt.Fprintf(&b, "  queue %s", ms(int64(v)))
		}
		if v, ok := tr.Attrs["ttft_ms"].(float64); ok && v > 0 {
			fmt.Fprintf(&b, "  first token %s", ms(int64(v)))
		}
		if v, ok := tr.Attrs["error"].(string); ok && v != "" {
			fmt.Fprintf(&b, "  error=%s", v)
		}
		fmt.Fprintf(&b, "  id=%s\n", tr.TraceID)
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

func (t traceOut) text() string {
	if len(t.Spans) == 0 {
		return "No such trace."
	}
	var b strings.Builder
	if t.Trace != nil {
		fmt.Fprintf(&b, "Trace %s: %s, %s, %d spans, status %s\n", t.Trace.TraceID, when(t.Trace.StartMS), ms(t.Trace.DurationMS), t.Trace.Spans, t.Trace.Status)
	}
	t0 := t.Spans[0].StartMS
	for _, sp := range t.Spans {
		if sp.StartMS < t0 {
			t0 = sp.StartMS
		}
	}
	depth := map[string]int{}
	byID := map[string]bool{}
	for _, sp := range t.Spans {
		byID[sp.SpanID] = true
	}
	for _, sp := range t.Spans {
		d, p := 0, sp.ParentID
		for p != "" && byID[p] && d < 20 {
			d++
			var next string
			for _, q := range t.Spans {
				if q.SpanID == p {
					next = q.ParentID
					break
				}
			}
			p = next
		}
		depth[sp.SpanID] = d
	}
	for _, sp := range t.Spans {
		fmt.Fprintf(&b, "%s%-10s %-10s +%-8s %-8s %s", strings.Repeat("  ", depth[sp.SpanID]), sp.Service, sp.Name, ms(sp.StartMS-t0), ms(sp.EndMS-sp.StartMS), sp.Status)
		keys := make([]string, 0, len(sp.Attrs))
		for k := range sp.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%v", k, sp.Attrs[k])
		}
		b.WriteByte('\n')
	}
	return b.String()
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
