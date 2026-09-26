// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// llm-hops: see every hop of a request through your local LLM system.
//
//	llm-hops serve  [-db hops.db] [-listen 127.0.0.1:11602] [-tail FILE] [-from-start] [-demo N] [-live] [-keep-days 14]
//	llm-hops tail   FILE [-to http://127.0.0.1:11602] [-from-start]     follow a yllm-gateway request log
//	llm-hops import FILE [-to URL]                                       a request log, once, oldest first
//	llm-hops demo   [-to URL] [-n 300] [-live]                           synthetic traffic
//	llm-hops version
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/adapters/yllmgateway"
	"github.com/YauhenBichel/llm-hops/internal/demo"
	"github.com/YauhenBichel/llm-hops/internal/model"
	"github.com/YauhenBichel/llm-hops/internal/server"
	"github.com/YauhenBichel/llm-hops/internal/store"
)

var version = "0.1.0"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "tail":
		err = tail(os.Args[2:])
	case "import":
		err = importLog(os.Args[2:])
	case "demo":
		err = demoCmd(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("llm-hops", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("llm-hops: %v", err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `llm-hops: see every hop of a request through your local LLM system.

  llm-hops serve  [-db hops.db] [-listen 127.0.0.1:11602] [-tail FILE]... [-from-start] [-demo N] [-live] [-keep-days 14]
  llm-hops tail   FILE [-to http://127.0.0.1:11602] [-from-start]
  llm-hops import FILE [-to http://127.0.0.1:11602]
  llm-hops demo   [-to http://127.0.0.1:11602] [-n 300] [-live]
  llm-hops version`)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := fs.String("db", "hops.db", "the SQLite file")
	listen := fs.String("listen", "127.0.0.1:11602", "address to listen on")
	var tails multi
	fs.Var(&tails, "tail", "follow a yllm-gateway request log (repeatable)")
	fromStart := fs.Bool("from-start", false, "read the whole file first, then follow")
	demoN := fs.Int("demo", 0, "load N synthetic traces first")
	live := fs.Bool("live", false, "with -demo: keep adding one trace a second")
	keepDays := fs.Float64("keep-days", 14, "drop traces older than this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	srv := server.New(st, server.NewBroadcast())
	if *demoN > 0 {
		g := demo.New(7, model.NowMS()-3_600_000, *demoN, 3600)
		for i := 0; i < *demoN; i++ {
			if _, err := st.Add(g.Next()); err != nil {
				return err
			}
		}
		log.Printf("demo: %d synthetic traces over the last hour", *demoN)
	}
	stop := make(chan struct{})
	for _, path := range tails {
		lines := make(chan string, 1000)
		go yllmgateway.Follow(path, *fromStart, 500*time.Millisecond, stop, lines)
		go func(path string) {
			batch := []model.Span{}
			flush := func() {
				if len(batch) == 0 {
					return
				}
				touched, err := st.Add(batch)
				if err != nil {
					log.Printf("tail %s: %v", path, err)
				}
				srv.Publish(touched)
				batch = batch[:0]
			}
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case l := <-lines:
					batch = append(batch, yllmgateway.LineToSpans(l)...)
					if len(batch) >= 2000 {
						flush()
					}
				case <-t.C:
					flush()
				case <-stop:
					flush()
					return
				}
			}
		}(path)
		log.Printf("following %s", path)
	}
	if *demoN > 0 && *live {
		go func() {
			g := demo.New(time.Now().UnixNano(), model.NowMS()-5000, 1, 1)
			for {
				select {
				case <-stop:
					return
				case <-time.After(1500 * time.Millisecond):
				}
				g2 := demo.New(time.Now().UnixNano(), model.NowMS()-5000, 1, 1)
				_ = g
				touched, err := st.Add(g2.Next())
				if err == nil {
					srv.Publish(touched)
				}
			}
		}()
	}
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Minute):
				if n, err := st.Prune(int64(*keepDays*86_400_000), model.NowMS()); err == nil && n > 0 {
					log.Printf("pruned %d traces older than %.1f days", n, *keepDays)
				}
			}
		}
	}()
	h := &http.Server{Addr: *listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		close(stop)
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = h.Shutdown(shutdown)
	}()
	log.Printf("llm-hops %s on http://%s/  (db %s)", version, *listen, *db)
	if err := h.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func tail(args []string) error {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	to := fs.String("to", "http://127.0.0.1:11602", "the llm-hops server")
	fromStart := fs.Bool("from-start", false, "read the whole file first, then follow")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("tail needs one file")
	}
	stop := make(chan struct{})
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); close(stop) }()
	lines := make(chan string, 1000)
	go yllmgateway.Follow(fs.Arg(0), *fromStart, 500*time.Millisecond, stop, lines)
	batch := []model.Span{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case l := <-lines:
			batch = append(batch, yllmgateway.LineToSpans(l)...)
			if len(batch) >= 1000 {
				post(*to, batch)
				batch = batch[:0]
			}
		case <-t.C:
			if len(batch) > 0 {
				post(*to, batch)
				batch = batch[:0]
			}
		case <-stop:
			if len(batch) > 0 {
				post(*to, batch)
			}
			return nil
		}
	}
}

func importLog(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	to := fs.String("to", "http://127.0.0.1:11602", "the llm-hops server")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("import needs one file")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	batch := []model.Span{}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		batch = append(batch, yllmgateway.LineToSpans(line)...)
		if len(batch) >= 2000 {
			n += post(*to, batch)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		n += post(*to, batch)
	}
	log.Printf("imported %d traces", n)
	return nil
}

func demoCmd(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	to := fs.String("to", "http://127.0.0.1:11602", "the llm-hops server")
	n := fs.Int("n", 300, "traces over the last hour")
	live := fs.Bool("live", false, "keep adding one trace a second")
	if err := fs.Parse(args); err != nil {
		return err
	}
	g := demo.New(7, model.NowMS()-3_600_000, *n, 3600)
	var spans []model.Span
	for i := 0; i < *n; i++ {
		spans = append(spans, g.Next()...)
	}
	post(*to, spans)
	log.Printf("sent %d synthetic traces to %s", *n, *to)
	if !*live {
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(1500 * time.Millisecond):
			g := demo.New(time.Now().UnixNano(), model.NowMS()-5000, 1, 1)
			post(*to, g.Next())
		}
	}
}

// post sends spans and returns the number of traces the server reports; it retries twice.
func post(to string, spans []model.Span) int {
	body, _ := json.Marshal(spans)
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := http.Post(strings.TrimRight(to, "/")+"/api/v1/spans", "application/json", bytes.NewReader(body)) //nolint:noctx
		if err == nil {
			var out struct {
				Traces []string `json:"traces"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return len(out.Traces)
			}
			log.Printf("llm-hops: server answered %s", resp.Status)
			return 0
		}
		if attempt == 2 {
			log.Printf("llm-hops: could not post %d spans to %s: %v", len(spans), to, err)
			return 0
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return 0
}
