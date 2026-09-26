// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package demo makes synthetic traffic that looks like a small home LLM system: a router on a laptop, a
// gateway with one slot, a model server that swaps models and loses its cache, and a few clients. For the
// demo page and the tests.
package demo

import (
	"math"
	"math/rand"

	"github.com/YauhenBichel/llm-hops/internal/model"
)

type modelDef struct {
	role, name string
	sizeGB     int
	weight     int
}

var models = []modelDef{
	{"coder", "qwen3-coder-next", 52, 55}, {"general", "gpt-oss:120b", 65, 20}, {"small", "gpt-oss:20b", 13, 10},
	{"embed", "bge-m3", 0, 10}, {"vision", "qwen3.5-35b", 24, 5},
}
var clients = []string{"editor", "agent", "scripts", "pipeline", "chat"}
var rules = []string{"LOC001 mechanical edit", "ESC002 root cause", "PRIV003 credential", "default"}

// Generator makes traces one at a time; it remembers which model is loaded.
type Generator struct {
	rng    *rand.Rand
	loaded string
	t      int64
	rate   float64 // traces per second, for the gaps
}

// New starts a generator at startMS, spreading n traces over spreadS seconds.
func New(seed int64, startMS int64, n int, spreadS float64) *Generator {
	return &Generator{rng: rand.New(rand.NewSource(seed)), loaded: "qwen3-coder-next", t: startMS, rate: float64(n) / spreadS} //nolint:gosec // not for security
}

func pick(rng *rand.Rand) modelDef {
	total := 0
	for _, m := range models {
		total += m.weight
	}
	r := rng.Intn(total)
	for _, m := range models {
		if r < m.weight {
			return m
		}
		r -= m.weight
	}
	return models[0]
}

func (g *Generator) between(lo, hi int) int64 { return int64(lo + g.rng.Intn(hi-lo+1)) }

// Next returns the spans of one trace.
func (g *Generator) Next() []model.Span {
	rng := g.rng
	g.t += int64(rng.ExpFloat64() / g.rate * 1000)
	t := g.t
	m := pick(rng)
	client := clients[rng.Intn(len(clients))]
	tid := model.NewID(16)
	var spans []model.Span
	cursor := t
	var routerRoot *model.Span
	if client == "editor" || client == "agent" {
		rr := model.Span{TraceID: tid, SpanID: model.NewID(8), Service: "router", Name: "request", StartMS: t, EndMS: t, Status: "ok",
			Attrs: model.Attrs{"client": client, "requested_model": "auto", "backend": "local", "provider": "ollama"}}
		routerRoot = &rr
		decide := g.between(1, 4)
		rule := rules[rng.Intn(len(rules))]
		// one request in six goes to the cloud: the router decides, the cloud answers, the gateway never sees it
		if rng.Float64() < 0.17 {
			rule = "ESC002 root cause"
			rr.Attrs["backend"], rr.Attrs["provider"], rr.Attrs["model"] = "cloud", "anthropic", "claude-fable-5-1"
			spans = append(spans, model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: rr.SpanID, Service: "router", Name: "decide",
				StartMS: cursor, EndMS: cursor + decide, Status: "ok", Attrs: model.Attrs{"rule": rule, "backend": "cloud"}})
			cursor += decide
			promptTokens := []float64{800, 2500, 6000}[rng.Intn(3)]
			completion := float64(rng.Intn(900) + 100)
			upMS := int64(1500 + rng.Float64()*6000)
			spans = append(spans, model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: rr.SpanID, Service: "anthropic", Name: "upstream",
				StartMS: cursor, EndMS: cursor + upMS, Status: "ok", Attrs: model.Attrs{"model": "claude-fable-5-1", "backend": "cloud",
					"prompt_tokens": promptTokens, "completion_tokens": completion, "ttft_ms": float64(g.between(600, 1400))}})
			cursor += upMS
			rr.EndMS = cursor + g.between(1, 3)
			rr.Attrs["status_code"] = 200.0
			rr.Attrs["prompt_tokens"], rr.Attrs["completion_tokens"] = promptTokens, completion
			rr.Attrs["rule"] = rule
			spans = append(spans, rr)
			return spans
		}
		spans = append(spans, model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: rr.SpanID, Service: "router", Name: "decide",
			StartMS: cursor, EndMS: cursor + decide, Status: "ok", Attrs: model.Attrs{"rule": rule, "backend": "local"}})
		rr.Attrs["rule"] = rule
		cursor += decide
	}
	wire := "openai"
	backend := "ollama"
	if m.role == "embed" {
		wire, backend = "embeddings", "cpu"
	}
	gw := model.Span{TraceID: tid, SpanID: model.NewID(8), Service: "gateway", Name: "request", StartMS: cursor, EndMS: cursor, Status: "ok",
		Attrs: model.Attrs{"client": client, "wire": wire, "role": m.role, "model": m.name, "requested_model": "role:" + m.role,
			"backend": backend, "stream": m.role != "embed"}}
	if routerRoot != nil {
		gw.ParentID = routerRoot.SpanID
	}
	promptTokens := int64(rng.Intn(880) + 20)
	if m.role != "embed" {
		promptTokens = []int64{300, 1200, 4000, 9000, 16000}[rng.Intn(5)]
	}
	var queueMS int64
	if rng.Float64() < 0.3 {
		queueMS = int64(rng.ExpFloat64() * 800)
	}
	if queueMS > 0 {
		spans = append(spans, model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: gw.SpanID, Service: "gateway", Name: "queue",
			StartMS: cursor, EndMS: cursor + queueMS, Status: "ok", Attrs: model.Attrs{"queue_ms": float64(queueMS), "waiting_for": g.loaded}})
		cursor += queueMS
	}
	checkMS := g.between(1, 3)
	tooLong := promptTokens > 12000 && rng.Float64() < 0.25
	check := model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: gw.SpanID, Service: "gateway", Name: "check",
		StartMS: cursor, EndMS: cursor + checkMS, Status: "ok", Attrs: model.Attrs{"prompt_tokens": float64(promptTokens)}}
	if tooLong {
		check.Status = "error"
		check.Attrs["error"] = "context_length"
	}
	spans = append(spans, check)
	cursor += checkMS
	status := "ok"
	statusCode := 200.0
	if tooLong {
		status = "error"
		statusCode = 400
		gw.Attrs["error"] = "context_length"
	} else {
		up := model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: gw.SpanID, Service: "ollama", Name: "upstream", StartMS: cursor, Status: "ok",
			Attrs: model.Attrs{"model": m.name, "backend": backend}}
		if m.role == "embed" {
			up.Service = "cpu"
		}
		upStart := cursor
		if m.role != "embed" && m.name != g.loaded {
			loadMS := int64(m.sizeGB) * g.between(500, 900)
			spans = append(spans, model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: up.SpanID, Service: "ollama", Name: "load",
				StartMS: cursor, EndMS: cursor + loadMS, Status: "ok", Attrs: model.Attrs{"model": m.name, "size_gb": float64(m.sizeGB), "evicted": g.loaded}})
			cursor += loadMS
			g.loaded = m.name
		}
		cacheLost := m.role != "embed" && rng.Float64() < 0.3
		var prefillMS int64
		switch {
		case m.role == "embed":
			prefillMS = int64(float64(promptTokens) * 1.1)
		case cacheLost:
			prefillMS = int64(float64(promptTokens) / (600 + rng.Float64()*300) * 1000)
		case promptTokens < 1500:
			prefillMS = int64(float64(promptTokens) / (150 + rng.Float64()*250) * 1000)
		default:
			prefillMS = g.between(150, 900)
		}
		cache := "hit"
		if cacheLost {
			cache = "lost"
		}
		spans = append(spans, model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: up.SpanID, Service: up.Service, Name: "prefill",
			StartMS: cursor, EndMS: cursor + prefillMS, Status: "ok", Attrs: model.Attrs{"prompt_tokens": float64(promptTokens), "cache": cache}})
		cursor += prefillMS
		ttft := cursor - upStart
		if m.role != "embed" {
			completion := int64(rng.Intn(580) + 20)
			tokS := 20 + rng.Float64()*20
			if m.role == "coder" {
				tokS = 35 + rng.Float64()*20
			}
			genMS := int64(float64(completion) / tokS * 1000)
			spans = append(spans, model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: up.SpanID, Service: up.Service, Name: "generate",
				StartMS: cursor, EndMS: cursor + genMS, Status: "ok", Attrs: model.Attrs{"completion_tokens": float64(completion), "tok_per_s": math.Round(tokS*10) / 10}})
			cursor += genMS
			gw.Attrs["completion_tokens"] = float64(completion)
			gw.Attrs["tok_per_s"] = math.Round(tokS*10) / 10
			gw.Attrs["ttft_ms"] = float64(ttft)
		}
		up.EndMS = cursor
		if rng.Float64() < 0.02 {
			up.Status = "error"
			up.Attrs["error"] = "timeout"
			status = "error"
			statusCode = 504
			gw.Attrs["error"] = "timeout"
		}
		gw.Attrs["prompt_tokens"] = float64(promptTokens)
		spans = append(spans, up)
		replyMS := g.between(1, 5)
		spans = append(spans, model.Span{TraceID: tid, SpanID: model.NewID(8), ParentID: gw.SpanID, Service: "gateway", Name: "reply",
			StartMS: cursor, EndMS: cursor + replyMS, Status: "ok", Attrs: model.Attrs{}})
		cursor += replyMS
	}
	gw.EndMS = cursor
	gw.Status = status
	gw.Attrs["status_code"] = statusCode
	gw.Attrs["queue_ms"] = float64(queueMS)
	spans = append(spans, gw)
	if routerRoot != nil {
		routerRoot.EndMS = cursor + g.between(1, 3)
		routerRoot.Status = status
		routerRoot.Attrs["status_code"] = statusCode
		routerRoot.Attrs["model"] = m.name
		spans = append(spans, *routerRoot)
	}
	return spans
}
