// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package yllmgateway

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// three real lines from a yllm-gateway request log of 26 September 2026 (no prompt text in the log by design)
const (
	lineEmbed  = `{"ts":"2026-09-26T05:19:21Z","wire":"embeddings","path":"/v1/embeddings","client":"127.0.0.1","requested_model":"role:embed","served_model":"bge-m3","role":"embed","route":null,"backend":"cpu","memory":null,"stream":false,"status":200,"queue_ms":0,"upstream_ms":49,"ttft_ms":null,"total_ms":53,"prompt_tokens":5,"completion_tokens":null,"tok_per_s":null,"fallback":false,"error":null,"key_id":"anon"}`
	lineChat   = `{"ts":"2026-09-11T07:17:52Z","wire":"responses","path":"/v1/responses","client":"127.0.0.1","requested_model":"yserver","served_model":"qwen3-coder:30b","role":"coder","route":"coder; text","backend":"ollama","stream":true,"status":200,"queue_ms":1200,"upstream_ms":5764,"ttft_ms":5564,"total_ms":6970,"prompt_tokens":12,"completion_tokens":14,"tok_per_s":70.0,"fallback":false,"error":null,"key_id":"anon"}`
	lineTooBig = `{"ts":"2026-09-12T07:05:10Z","wire":"openai","path":"/v1/chat/completions","client":"127.0.0.1","requested_model":"role:coder","served_model":"qwen3-coder-next","role":"coder","route":null,"backend":"ollama","memory":null,"stream":false,"status":400,"queue_ms":0,"upstream_ms":null,"ttft_ms":null,"total_ms":210,"prompt_tokens":null,"completion_tokens":null,"tok_per_s":null,"fallback":false,"error":"context_length","key_id":"anon"}`
)

func find(spans []spanT, name string) *spanT {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

type spanT = struct {
	TraceID, SpanID, ParentID, Service, Name string
	StartMS, EndMS                           int64
	Status                                   string
	Attrs                                    map[string]any
}

func toT(spans []spanT) []spanT { return spans }

func TestChatLineBecomesQueueUpstreamReply(t *testing.T) {
	spans := LineToSpans(lineChat)
	if len(spans) != 4 {
		t.Fatalf("want root, queue, upstream, reply; got %d spans: %+v", len(spans), spans)
	}
	root := spans[0]
	start := time.Date(2026, 9, 11, 7, 17, 52, 0, time.UTC).UnixMilli()
	if root.Name != "request" || root.Service != "gateway" || root.StartMS != start || root.EndMS != start+6970 {
		t.Fatalf("root wrong: %+v", root)
	}
	if root.Attrs["model"] != "qwen3-coder:30b" || root.Attrs["status_code"] != float64(200) || root.Attrs["client"] != "127.0.0.1" {
		t.Fatalf("root attrs wrong: %v", root.Attrs)
	}
	var q, up, reply *spanT
	for i := range spans {
		s := spanT{spans[i].TraceID, spans[i].SpanID, spans[i].ParentID, spans[i].Service, spans[i].Name, spans[i].StartMS, spans[i].EndMS, spans[i].Status, spans[i].Attrs}
		switch s.Name {
		case "queue":
			q = &s
		case "upstream":
			up = &s
		case "reply":
			reply = &s
		}
	}
	if q == nil || q.StartMS != start || q.EndMS != start+1200 || q.ParentID != "root" {
		t.Fatalf("queue wrong: %+v", q)
	}
	if up == nil || up.Service != "ollama" || up.StartMS != start+1200 || up.EndMS != start+1200+5764 || up.Attrs["ttft_ms"] != float64(5564) {
		t.Fatalf("upstream wrong: %+v", up)
	}
	if reply == nil || reply.StartMS != start+1200+5764 || reply.EndMS != start+6970 {
		t.Fatalf("reply wrong: %+v", reply)
	}
}

func TestEmbeddingLineHasNoQueueAndCPUBackend(t *testing.T) {
	spans := LineToSpans(lineEmbed)
	names := map[string]bool{}
	for _, s := range spans {
		names[s.Name] = true
		if s.Name == "upstream" && s.Service != "cpu" {
			t.Fatalf("the embedding ran on the cpu backend, got service %q", s.Service)
		}
	}
	if names["queue"] || !names["upstream"] || !names["reply"] {
		t.Fatalf("names: %v", names)
	}
}

func TestRefusedPromptIsAnErrorWithACheckSpanAndNoUpstream(t *testing.T) {
	spans := LineToSpans(lineTooBig)
	if spans[0].Status != "error" || spans[0].Attrs["error"] != "context_length" {
		t.Fatalf("root: %+v", spans[0])
	}
	var check, up bool
	for _, s := range spans {
		if s.Name == "check" && s.Status == "error" {
			check = true
		}
		if s.Name == "upstream" {
			up = true
		}
	}
	if !check || up {
		t.Fatalf("check=%v upstream=%v", check, up)
	}
}

func TestSameLineSameTraceID(t *testing.T) {
	a, b := LineToSpans(lineChat), LineToSpans(lineChat)
	if a[0].TraceID != b[0].TraceID || len(a[0].TraceID) != 32 {
		t.Fatalf("ids differ or wrong length: %q %q", a[0].TraceID, b[0].TraceID)
	}
	if LineToSpans(lineEmbed)[0].TraceID == a[0].TraceID {
		t.Fatal("different lines share an id")
	}
}

func TestGarbageLinesAreSkipped(t *testing.T) {
	for _, l := range []string{"", "   ", "not json", `{"a":1}`, `{"ts":"nope","wire":"openai"}`, `[1,2]`} {
		if got := LineToSpans(l); got != nil {
			t.Fatalf("%q gave %v", l, got)
		}
	}
}

func TestFollowSeesAppendsAndSurvivesRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.jsonl")
	if err := os.WriteFile(path, []byte("old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	out := make(chan string, 10)
	go Follow(path, false, 20*time.Millisecond, stop, out)
	time.Sleep(60 * time.Millisecond) // let it open the file at its end
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("one\n")
	f.WriteString("two")
	f.Close()
	if got := <-out; got != "one\n" {
		t.Fatalf("got %q", got)
	}
	select {
	case got := <-out:
		t.Fatalf("a partial line was delivered: %q", got)
	case <-time.After(80 * time.Millisecond):
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("\n")
	f.Close()
	if got := <-out; got != "two\n" {
		t.Fatalf("got %q", got)
	}
	// rotation: the file is renamed and a new one appears
	os.Rename(path, path+".1")
	os.WriteFile(path, []byte("three\n"), 0o600)
	select {
	case got := <-out:
		if got != "three\n" {
			t.Fatalf("after rotation got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing after rotation")
	}
}

func TestALineWithATraceIDUsesItAndTheGatewaysSpanIDs(t *testing.T) {
	line := `{"ts":"2026-09-26T15:42:25.291Z","wire":"openai","path":"/v1/chat/completions","client":"127.0.0.1","requested_model":"role:coder","served_model":"qwen3-coder-next","role":"coder","backend":"ollama","stream":false,"status":200,"queue_ms":10,"upstream_ms":500,"ttft_ms":null,"total_ms":520,"prompt_tokens":5,"completion_tokens":3,"tok_per_s":6.0,"fallback":false,"error":null,"key_id":"anon","trace_id":"0123456789abcdef0123456789abcdef"}`
	spans := LineToSpans(line)
	if spans[0].TraceID != "0123456789abcdef0123456789abcdef" || spans[0].SpanID != "0123456789abcdef" {
		t.Fatalf("root: %+v", spans[0])
	}
	if spans[0].StartMS != time.Date(2026, 9, 26, 15, 42, 25, 291_000_000, time.UTC).UnixMilli() {
		t.Fatalf("the milliseconds of ts were lost: %d", spans[0].StartMS)
	}
	for _, s := range spans[1:] {
		if s.ParentID != "0123456789abcdef" {
			t.Fatalf("child parent: %+v", s)
		}
	}
}

