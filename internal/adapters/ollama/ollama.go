// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package ollama turns Ollama's own log into model loads: every "loading model via llama-server" line opens
// a load, the "model loaded" (or "runner started") line closes it, and the pair becomes one trace of its
// own: service ollama, name load, with the model's name, the blob, and the seconds it took. The picture
// this gives is "who else is using the GPU": every swap, with its cost, as a bar on the time axis.
//
// The log is read from journalctl (`llm-hops serve -ollama-journal`, which runs `journalctl -u ollama -f`)
// or from any reader of the same lines. Blob hashes become model names through Ollama's API (/api/tags and
// /api/show), cached; a blob no model claims is shown by its hash.
package ollama

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/model"
)

// Service is the name the load spans carry.
const Service = "ollama"

var (
	reTime  = regexp.MustCompile(`time=(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z)`)
	reStart = regexp.MustCompile(`msg="loading model via llama-server" model=\S*?(sha256-[0-9a-f]{12})`)
	reDone  = regexp.MustCompile(`msg="llama(?:-server| runner) started in ([\d.]+) seconds"|llama_server: model loaded|msg="loaded runners"`)
	reISO   = regexp.MustCompile(`^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:?\d\d))`)
	reUnl   = regexp.MustCompile(`msg="(?:runner (?:expired|unloaded)|unloading model|removing model)[^"]*"`)
)

// Namer maps a blob hash to a model name.
type Namer interface {
	Name(blob string) string
}

// Parser keeps the open load between lines.
type Parser struct {
	Names   Namer
	open    *load
	lastAt  time.Time
	LoadsMS int64 // for tests and status: total time spent loading
}

type load struct {
	blob  string
	start time.Time
}

// Line reads one journal line and returns a finished load as spans, or nil.
func (p *Parser) Line(line string) []model.Span {
	at := when(line, p.lastAt)
	p.lastAt = at
	if m := reStart.FindStringSubmatch(line); m != nil {
		p.open = &load{blob: m[1], start: at}
		return nil
	}
	if p.open != nil && reDone.MatchString(line) {
		if strings.Contains(line, "loaded runners") && at.Sub(p.open.start) < 500*time.Millisecond {
			return nil // the count line right after the start, not the end of the load
		}
		l := p.open
		p.open = nil
		end := at
		if m := reDone.FindStringSubmatch(line); m != nil && m[1] != "" {
			if secs, err := parseSeconds(m[1]); err == nil {
				end = l.start.Add(secs)
			}
		}
		if end.Before(l.start) {
			end = l.start
		}
		name := l.blob
		if p.Names != nil {
			if n := p.Names.Name(l.blob); n != "" {
				name = n
			}
		}
		dur := end.Sub(l.start).Milliseconds()
		p.LoadsMS += dur
		tid := model.NewID(16)
		return []model.Span{{
			TraceID: tid, SpanID: tid[:16], Service: Service, Name: "load", StartMS: l.start.UnixMilli(), EndMS: end.UnixMilli(),
			Status: "ok", Attrs: model.Attrs{"model": name, "blob": l.blob, "kind": "load", "load_ms": float64(dur), "backend": "ollama"},
		}}
	}
	if reUnl.MatchString(line) {
		tid := model.NewID(16)
		return []model.Span{{
			TraceID: tid, SpanID: tid[:16], Service: Service, Name: "unload", StartMS: at.UnixMilli(), EndMS: at.UnixMilli(),
			Status: "ok", Attrs: model.Attrs{"kind": "unload", "backend": "ollama"},
		}}
	}
	return nil
}

func when(line string, fallback time.Time) time.Time {
	if m := reTime.FindStringSubmatch(line); m != nil {
		if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			return t
		}
	}
	if m := reISO.FindStringSubmatch(line); m != nil {
		if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			return t
		}
		if t, err := time.Parse("2006-01-02T15:04:05-0700", m[1]); err == nil {
			return t
		}
	}
	if fallback.IsZero() {
		return time.Now()
	}
	return fallback
}

func parseSeconds(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s + "s")
	return d, err
}

// APINamer resolves blobs through Ollama's API, refreshing its table when it meets a blob it does not know.
type APINamer struct {
	Base   string
	Client *http.Client
	mu     sync.Mutex
	table  map[string]string
	last   time.Time
}

// Name returns the model name for a blob prefix, or "".
func (n *APINamer) Name(blob string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if name, ok := n.table[blob]; ok {
		return name
	}
	if time.Since(n.last) > 30*time.Second {
		n.refresh()
		n.last = time.Now()
	}
	return n.table[blob]
}

func (n *APINamer) refresh() {
	if n.Client == nil {
		n.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if n.table == nil {
		n.table = map[string]string{}
	}
	resp, err := n.Client.Get(strings.TrimRight(n.Base, "/") + "/api/tags")
	if err != nil {
		return
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tags)
	resp.Body.Close()
	for _, m := range tags.Models {
		body := strings.NewReader(`{"name":` + jsonString(m.Name) + `}`)
		r, err := n.Client.Post(strings.TrimRight(n.Base, "/")+"/api/show", "application/json", body)
		if err != nil {
			continue
		}
		var show struct {
			Modelfile string `json:"modelfile"`
		}
		_ = json.NewDecoder(r.Body).Decode(&show)
		r.Body.Close()
		for _, hit := range regexp.MustCompile(`sha256-([0-9a-f]{12})`).FindAllStringSubmatch(show.Modelfile, -1) {
			if _, taken := n.table["sha256-"+hit[1]]; !taken {
				n.table["sha256-"+hit[1]] = m.Name
			}
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Follow runs `journalctl -u ollama -f -o cat` (plus what is there since `since`) and sends every finished
// load's spans to out until ctx ends.
func Follow(ctx context.Context, since string, names Namer, out chan<- []model.Span) error {
	args := []string{"-u", "ollama", "-f", "-o", "short-iso", "--no-pager"}
	if since != "" {
		args = append(args, "--since", since)
	} else {
		args = append(args, "-n", "0")
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	p := &Parser{Names: names}
	feed(pipe, p, out)
	return cmd.Wait()
}

func feed(r io.Reader, p *Parser, out chan<- []model.Span) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		if spans := p.Line(sc.Text()); spans != nil {
			out <- spans
		}
	}
}

// Feed reads lines from any reader (a saved journal, a test) into the parser.
func Feed(r io.Reader, p *Parser, out chan<- []model.Span) { feed(r, p, out) }
