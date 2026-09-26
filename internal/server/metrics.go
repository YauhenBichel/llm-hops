// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/YauhenBichel/llm-hops/internal/model"
)

// metrics renders the last five minutes in Prometheus' text format: what a Grafana panel or an alert rule
// needs, without a client library. Percentiles are computed on the window, not estimated from buckets.
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	now := model.NowMS()
	st, err := s.Store.Stats(now-300_000, now, 60_000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	line := func(name, help, typ string, rows ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, r := range rows {
			b.WriteString(r)
			b.WriteByte('\n')
		}
	}
	line("llm_hops_spans_received_total", "Spans accepted since the process started.", "counter",
		fmt.Sprintf("llm_hops_spans_received_total %d", s.spansReceived.Load()))
	line("llm_hops_spans_rejected_total", "Spans rejected as malformed since the process started.", "counter",
		fmt.Sprintf("llm_hops_spans_rejected_total %d", s.spansRejected.Load()))
	line("llm_hops_requests_5m", "Requests (traces) that started in the last five minutes.", "gauge",
		fmt.Sprintf("llm_hops_requests_5m %d", st.Requests))
	line("llm_hops_errors_5m", "Requests with an error in the last five minutes.", "gauge",
		fmt.Sprintf("llm_hops_errors_5m %d", st.Errors))
	line("llm_hops_model_switches_5m", "Changes of the served model between consecutive requests in the last five minutes.", "gauge",
		fmt.Sprintf("llm_hops_model_switches_5m %d", st.ModelSwitches))
	line("llm_hops_request_ms", "Request duration percentiles over the last five minutes.", "gauge",
		fmt.Sprintf("llm_hops_request_ms{quantile=\"0.5\"} %d", st.P50MS),
		fmt.Sprintf("llm_hops_request_ms{quantile=\"0.95\"} %d", st.P95MS),
		fmt.Sprintf("llm_hops_request_ms{quantile=\"1\"} %d", st.MaxMS))
	hops := make([]string, 0, len(st.Hops))
	for name := range st.Hops {
		hops = append(hops, name)
	}
	sort.Strings(hops)
	rows := []string{}
	for _, name := range hops {
		h := st.Hops[name]
		rows = append(rows,
			fmt.Sprintf("llm_hops_hop_ms{hop=%q,quantile=\"0.5\"} %d", name, h.P50MS),
			fmt.Sprintf("llm_hops_hop_ms{hop=%q,quantile=\"0.95\"} %d", name, h.P95MS))
	}
	line("llm_hops_hop_ms", "Per-hop duration percentiles over the last five minutes (leaf hops).", "gauge", rows...)
	rows = rows[:0]
	for _, m := range st.Models {
		rows = append(rows,
			fmt.Sprintf("llm_hops_model_requests_5m{model=%q} %d", m.Model, m.Requests),
			fmt.Sprintf("llm_hops_model_errors_5m{model=%q} %d", m.Model, m.Errors),
			fmt.Sprintf("llm_hops_model_request_ms{model=%q,quantile=\"0.95\"} %d", m.Model, m.P95MS))
	}
	line("llm_hops_model_requests_5m", "Requests per served model in the last five minutes (with errors and p95 as neighbours).", "gauge", rows...)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
