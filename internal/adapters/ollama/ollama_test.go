// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package ollama

import (
	"strings"
	"testing"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/model"
)

type names map[string]string

func (n names) Name(b string) string { return n[b] }

// real lines from the box's journal of 26 September 2026 (short-iso output), one load
const journal = `2026-09-26T20:00:23+00:00 yserver ollama[4426]: time=2026-09-26T20:00:23.239Z level=INFO source=sched.go:735 msg="loaded runners" count=1
2026-09-26T20:01:12+00:00 yserver ollama[4426]: time=2026-09-26T20:01:12.390Z level=INFO source=llama_server.go:1048 msg="loading model via llama-server" model=/usr/share/ollama/.ollama/models/blobs/sha256-280af6832eca23cb322c4dcc65edfea98a21b8f8ab07dc7553bd6f7e6e7a3313
2026-09-26T20:01:12+00:00 yserver ollama[4426]: time=2026-09-26T20:01:12.644Z level=INFO source=llama_server.go:1350 msg="waiting for llama-server to become available" status="llm server loading model"
2026-09-26T20:01:15+00:00 yserver ollama[4426]: load_tensors: offloaded 61/61 layers to GPU
2026-09-26T20:01:22+00:00 yserver ollama[4426]: srv  llama_server: model loaded
2026-09-26T20:01:22+00:00 yserver ollama[4426]: time=2026-09-26T20:01:22.356Z level=INFO source=sched.go:735 msg="loaded runners" count=1
`

func TestOneLoadBecomesOneTrace(t *testing.T) {
	p := &Parser{Names: names{"sha256-280af6832eca": "gemma4:31b"}}
	var got [][]model.Span
	for _, line := range strings.Split(journal, "\n") {
		if s := p.Line(line); s != nil {
			got = append(got, s)
		}
	}
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("got %d traces: %+v", len(got), got)
	}
	s := got[0][0]
	start := time.Date(2026, 9, 26, 20, 1, 12, 390_000_000, time.UTC).UnixMilli()
	end := time.Date(2026, 9, 26, 20, 1, 22, 0, time.UTC).UnixMilli()
	if s.Name != "load" || s.Service != "ollama" || s.StartMS != start || s.EndMS != end {
		t.Fatalf("%+v (want %d..%d)", s, start, end)
	}
	if s.Attrs["model"] != "gemma4:31b" || s.Attrs["kind"] != "load" || s.Attrs["blob"] != "sha256-280af6832eca" {
		t.Fatalf("attrs: %v", s.Attrs)
	}
	if p.LoadsMS != end-start {
		t.Fatalf("LoadsMS %d", p.LoadsMS)
	}
}

func TestStartedInSecondsSetsTheEndAndUnknownBlobKeepsItsHash(t *testing.T) {
	p := &Parser{}
	p.Line(`2026-09-26T10:00:00+00:00 yserver ollama[1]: time=2026-09-26T10:00:00.000Z level=INFO msg="loading model via llama-server" model=/x/blobs/sha256-abcdefabcdef0000`)
	s := p.Line(`2026-09-26T10:00:11+00:00 yserver ollama[1]: time=2026-09-26T10:00:11.000Z level=INFO msg="llama-server started in 9.83 seconds"`)
	if s == nil || s[0].EndMS-s[0].StartMS != 9830 || s[0].Attrs["model"] != "sha256-abcdefabcdef" {
		t.Fatalf("%+v", s)
	}
	if p.Line("garbage") != nil || p.Line(`time=2026-09-26T10:00:12Z msg="loaded runners" count=1`) != nil {
		t.Fatal("a line with no open load must give nothing")
	}
}
