// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package otlp

import "testing"

// what the OpenTelemetry Python SDK's OTLP/HTTP exporter sends (JSON encoding), with GenAI attributes
const sample = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"my-agent"}}]},
 "scopeSpans":[{"scope":{"name":"openai"},"spans":[
  {"traceId":"5B8EFFF798038103D269B633813FC60C","spanId":"EEE19B7EC3C1B174","parentSpanId":"","name":"chat claude",
   "startTimeUnixNano":"1790409578748000000","endTimeUnixNano":"1790409580748000000",
   "attributes":[{"key":"gen_ai.request.model","value":{"stringValue":"claude-fable-5-1"}},
                 {"key":"gen_ai.system","value":{"stringValue":"anthropic"}},
                 {"key":"gen_ai.usage.input_tokens","value":{"intValue":"1200"}},
                 {"key":"gen_ai.usage.output_tokens","value":{"intValue":"80"}},
                 {"key":"gen_ai.prompt","value":{"stringValue":"the secret question"}},
                 {"key":"temperature","value":{"doubleValue":0.2}},
                 {"key":"stream","value":{"boolValue":true}}],
   "status":{"code":2,"message":"boom"}},
  {"traceId":"5B8EFFF798038103D269B633813FC60C","spanId":"0000000000000002","parentSpanId":"EEE19B7EC3C1B174","name":"tool read_file",
   "startTimeUnixNano":"1790409579000000000","endTimeUnixNano":"1790409579100000000","status":{}},
  {"traceId":"","spanId":"x","name":"no trace id","startTimeUnixNano":"1"}
 ]}]}]}`

func TestDecodeMapsGenAIAndDropsPrompts(t *testing.T) {
	spans, skipped, err := Decode([]byte(sample))
	if err != nil || skipped != 1 || len(spans) != 2 {
		t.Fatalf("spans=%d skipped=%d err=%v", len(spans), skipped, err)
	}
	s := spans[0]
	if s.Service != "my-agent" || s.Name != "chat claude" || s.TraceID != "5b8efff798038103d269b633813fc60c" || s.SpanID != "eee19b7ec3c1b174" {
		t.Fatalf("%+v", s)
	}
	if s.StartMS != 1790409578748 || s.EndMS != 1790409580748 || s.Status != "error" {
		t.Fatalf("%+v", s)
	}
	if s.Attrs["model"] != "claude-fable-5-1" || s.Attrs["provider"] != "anthropic" || s.Attrs["prompt_tokens"] != 1200.0 ||
		s.Attrs["completion_tokens"] != 80.0 || s.Attrs["temperature"] != 0.2 || s.Attrs["stream"] != true {
		t.Fatalf("attrs: %v", s.Attrs)
	}
	if _, ok := s.Attrs["gen_ai.prompt"]; ok {
		t.Fatal("the prompt text was kept")
	}
	if spans[1].ParentID != "eee19b7ec3c1b174" || spans[1].Status != "ok" || spans[1].EndMS-spans[1].StartMS != 100 {
		t.Fatalf("child: %+v", spans[1])
	}
}

func TestDecodeRejectsGarbageAndAcceptsEmpty(t *testing.T) {
	if _, _, err := Decode([]byte("nope")); err == nil {
		t.Fatal("garbage accepted")
	}
	spans, _, err := Decode([]byte(`{"resourceSpans":[]}`))
	if err != nil || len(spans) != 0 {
		t.Fatal(err, spans)
	}
}
