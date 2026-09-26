// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package yllmgateway turns yllm-gateway's request log, one JSON line per request, into one trace per line:
//
//	request (gateway)               ts .. ts + total_ms
//	  queue (gateway)               0 .. queue_ms                  the wait for a slot, if any
//	  upstream (<backend>)          queue_ms .. queue_ms + upstream_ms
//	    first token at ttft_ms      as an attribute, drawn as a marker
//	  reply (gateway)               the rest, if the total is longer than queue + upstream
//
// The log's ts has second resolution; the durations are milliseconds. So every span in a trace is placed
// exactly relative to the request's start, and the start itself is known to the second. The trace id is a
// hash of the line, so following the same file twice produces the same traces, never duplicates.
//
// The line's fields (yllm-gateway, September 2026): ts, wire, path, client, requested_model, served_model,
// role, route, backend, memory, stream, status, queue_ms, upstream_ms, ttft_ms, total_ms, prompt_tokens,
// completion_tokens, tok_per_s, fallback, error, key_id. No prompt or completion text is in the log, so
// none reaches llm-hops.
package yllmgateway

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/model"
)

var rootAttrs = []string{"wire", "path", "client", "requested_model", "role", "route", "backend", "memory", "stream",
	"prompt_tokens", "completion_tokens", "tok_per_s", "fallback", "error", "key_id",
	"queue_ms", "upstream_ms", "ttft_ms", "total_ms"}

// Service is the name the gateway's own spans carry.
const Service = "gateway"

// LineToSpans converts one log line to its spans; nil for a line that is not a request record.
func LineToSpans(line string) []model.Span {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return nil
	}
	ts, ok := rec["ts"].(string)
	if !ok || rec["wire"] == nil {
		return nil
	}
	start, ok := parseTS(ts)
	if !ok {
		return nil
	}
	sum := sha1.Sum([]byte(line)) //nolint:gosec // an identifier, not a secret
	tid := hex.EncodeToString(sum[:])[:32]
	total := num(rec["total_ms"])
	queue := num(rec["queue_ms"])
	upstream, hasUpstream := rec["upstream_ms"].(float64)
	statusCode := num(rec["status"])
	errKind, _ := rec["error"].(string)
	status := "ok"
	if statusCode >= 500 || errKind != "" {
		status = "error"
	}
	attrs := model.Attrs{}
	for _, k := range rootAttrs {
		if v, ok := rec[k]; ok && v != nil {
			attrs[k] = v
		}
	}
	modelName := strOr(rec["served_model"], strOr(rec["requested_model"], "?"))
	attrs["model"] = modelName
	attrs["status_code"] = float64(statusCode)
	end := start + max(total, queue+int64(upstream))
	spans := []model.Span{{TraceID: tid, SpanID: "root", Service: Service, Name: "request", StartMS: start, EndMS: end, Status: status, Attrs: attrs}}
	t := start
	if queue > 0 {
		spans = append(spans, model.Span{TraceID: tid, SpanID: "queue", ParentID: "root", Service: Service, Name: "queue",
			StartMS: t, EndMS: t + queue, Status: "ok", Attrs: model.Attrs{"queue_ms": float64(queue)}})
	}
	t += queue
	if hasUpstream {
		backend := strOr(rec["backend"], "ollama")
		up := model.Attrs{"model": modelName, "backend": backend}
		for _, k := range []string{"ttft_ms", "completion_tokens", "tok_per_s"} {
			if v, ok := rec[k]; ok && v != nil {
				up[k] = v
			}
		}
		upStatus := "ok"
		switch errKind {
		case "timeout", "connect", "status_5xx", "stream", "embedder":
			upStatus = "error"
		}
		if errKind != "" {
			up["error"] = errKind
		}
		spans = append(spans, model.Span{TraceID: tid, SpanID: "upstream", ParentID: "root", Service: backend, Name: "upstream",
			StartMS: t, EndMS: t + int64(upstream), Status: upStatus, Attrs: up})
		t += int64(upstream)
	}
	if errKind == "context_length" {
		spans = append(spans, model.Span{TraceID: tid, SpanID: "check", ParentID: "root", Service: Service, Name: "check",
			StartMS: start + queue, EndMS: start + queue + 1, Status: "error", Attrs: model.Attrs{"error": "context_length"}})
	}
	if total > (t-start)+1 {
		spans = append(spans, model.Span{TraceID: tid, SpanID: "reply", ParentID: "root", Service: Service, Name: "reply",
			StartMS: t, EndMS: start + total, Status: "ok", Attrs: model.Attrs{}})
	}
	return spans
}

// Follow yields lines as they are appended to path, surviving the log's rotation (the file is reopened when
// its inode changes or it shrinks). fromStart reads what is there first; otherwise it starts at the end.
// It returns when stop is closed. Lines are sent whole; a partial write waits for its newline.
func Follow(path string, fromStart bool, poll time.Duration, stop <-chan struct{}, out chan<- string) {
	var f *os.File
	var rd *bufio.Reader
	var ino uint64
	var pos int64
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	wait := func() bool {
		select {
		case <-stop:
			return false
		case <-time.After(poll):
			return true
		}
	}
	for {
		st, err := os.Stat(path)
		if err != nil {
			if !wait() {
				return
			}
			continue
		}
		curIno := inode(st)
		if f == nil || curIno != ino || st.Size() < pos {
			if f != nil {
				f.Close()
			}
			f, err = os.Open(path)
			if err != nil {
				if !wait() {
					return
				}
				continue
			}
			ino = curIno
			pos = 0
			if !fromStart && rd == nil { // the very first open: start at the end
				pos, _ = f.Seek(0, io.SeekEnd)
			}
			rd = bufio.NewReader(f)
		}
		line, err := rd.ReadString('\n')
		if err == nil {
			pos += int64(len(line))
			select {
			case out <- line:
			case <-stop:
				return
			}
			continue
		}
		if line != "" { // a partial line: rewind to its start and wait
			if _, err := f.Seek(pos, io.SeekStart); err == nil {
				rd = bufio.NewReader(f)
			}
		}
		if !wait() {
			return
		}
	}
}

func parseTS(ts string) (int64, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

func num(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}
