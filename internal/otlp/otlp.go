// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package otlp accepts OpenTelemetry traces in OTLP/JSON (the protocol's HTTP+JSON encoding, what every
// OpenTelemetry SDK's OTLP exporter can send to `<server>/v1/traces`) and turns them into llm-hops spans:
//
//	resource attribute service.name  -> service
//	span name                        -> name
//	traceId, spanId, parentSpanId    -> the ids (hex as sent)
//	startTimeUnixNano, endTimeUnixNano -> start_ms, end_ms
//	status.code STATUS_CODE_ERROR (2) -> status error
//	attributes                       -> attrs, flattened; OpenTelemetry's GenAI names are mapped to the
//	                                    page's: gen_ai.request.model -> model, gen_ai.usage.input_tokens ->
//	                                    prompt_tokens, gen_ai.usage.output_tokens -> completion_tokens
//
// No prompt or completion text is kept: gen_ai.prompt, gen_ai.completion and the event bodies are dropped.
package otlp

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/YauhenBichel/llm-hops/internal/model"
)

// The GenAI semantic conventions that have a name on the page.
var genAI = map[string]string{
	"gen_ai.request.model":           "model",
	"gen_ai.response.model":          "model",
	"gen_ai.system":                  "provider",
	"gen_ai.provider.name":           "provider",
	"gen_ai.usage.input_tokens":      "prompt_tokens",
	"gen_ai.usage.output_tokens":     "completion_tokens",
	"gen_ai.usage.prompt_tokens":     "prompt_tokens",
	"gen_ai.usage.completion_tokens": "completion_tokens",
	"gen_ai.request.is_stream":       "stream",
	"http.response.status_code":      "status_code",
	"http.status_code":               "status_code",
	"client.address":                 "client",
	"enduser.id":                     "client",
	"error.type":                     "error",
}

// dropped are attribute prefixes that carry text or bodies.
var dropped = []string{"gen_ai.prompt", "gen_ai.completion", "gen_ai.input.messages", "gen_ai.output.messages",
	"gen_ai.system_instructions", "llm.prompts", "llm.completions", "input.value", "output.value"}

type anyValue struct {
	StringValue *string   `json:"stringValue"`
	IntValue    *string   `json:"intValue"` // OTLP/JSON encodes int64 as a decimal string
	IntNumber   *float64  `json:"-"`
	DoubleValue *float64  `json:"doubleValue"`
	BoolValue   *bool     `json:"boolValue"`
	ArrayValue  *struct{} `json:"arrayValue"`
}

type keyValue struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type otlpSpan struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	ParentSpanID string     `json:"parentSpanId"`
	Name         string     `json:"name"`
	Start        string     `json:"startTimeUnixNano"`
	End          string     `json:"endTimeUnixNano"`
	Attributes   []keyValue `json:"attributes"`
	Status       struct {
		Code json.RawMessage `json:"code"`
	} `json:"status"`
}

type request struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []keyValue `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []otlpSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// Decode turns an OTLP/JSON ExportTraceServiceRequest into spans. Spans without a trace id or a start are skipped.
func Decode(body []byte) ([]model.Span, int, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, 0, err
	}
	var out []model.Span
	skipped := 0
	for _, rs := range req.ResourceSpans {
		service := "otel"
		for _, kv := range rs.Resource.Attributes {
			if kv.Key == "service.name" {
				if v, ok := value(kv.Value).(string); ok && v != "" {
					service = v
				}
			}
		}
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				start, okS := nanos(s.Start)
				if s.TraceID == "" || !okS {
					skipped++
					continue
				}
				end, okE := nanos(s.End)
				if !okE || end < start {
					end = start
				}
				attrs := model.Attrs{}
				for _, kv := range s.Attributes {
					if isDropped(kv.Key) {
						continue
					}
					v := value(kv.Value)
					if v == nil {
						continue
					}
					key := kv.Key
					if mapped, ok := genAI[key]; ok {
						key = mapped
					}
					if str, ok := v.(string); ok && len(str) > 512 {
						v = str[:512]
					}
					attrs[key] = v
				}
				status := "ok"
				if code := strings.Trim(string(s.Status.Code), `"`); code == "2" || code == "STATUS_CODE_ERROR" {
					status = "error"
				}
				out = append(out, model.Span{TraceID: strings.ToLower(s.TraceID), SpanID: strings.ToLower(s.SpanID), ParentID: strings.ToLower(s.ParentSpanID),
					Service: service, Name: s.Name, StartMS: start, EndMS: end, Status: status, Attrs: attrs})
			}
		}
	}
	return out, skipped, nil
}

func isDropped(key string) bool {
	for _, p := range dropped {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func nanos(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, err2 := strconv.ParseFloat(s, 64)
		if err2 != nil {
			return 0, false
		}
		n = int64(f)
	}
	return n / 1_000_000, true
}

// value reads an OTLP AnyValue: strings, ints (as strings), doubles, bools; anything else is dropped.
func value(raw json.RawMessage) any {
	var v struct {
		StringValue *string      `json:"stringValue"`
		IntValue    *json.Number `json:"intValue"`
		DoubleValue *float64     `json:"doubleValue"`
		BoolValue   *bool        `json:"boolValue"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		// intValue may arrive as a bare number in some encoders
		var alt struct {
			IntValue *float64 `json:"intValue"`
		}
		if json.Unmarshal(raw, &alt) == nil && alt.IntValue != nil {
			return *alt.IntValue
		}
		return nil
	}
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		if f, err := v.IntValue.Float64(); err == nil {
			return f
		}
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.BoolValue != nil:
		return *v.BoolValue
	}
	return nil
}
