// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// llm-hops: see every hop of a request through your local LLM system.
//
//	llm-hops serve  [-config hops.toml] [-db hops.db] [-listen 127.0.0.1:11602] [-tail FILE] [-from-start] [-demo N] [-live] [-keep-days 14]
//	llm-hops tail   FILE [-to http://127.0.0.1:11602] [-from-start]     follow a yllm-gateway request log
//	llm-hops import FILE [-to URL]                                       a request log, or an export, once
//	llm-hops export [-db hops.db] [-since MS] [-until MS] > spans.jsonl  every span, oldest first
//	llm-hops demo   [-to URL] [-n 300] [-live]                           synthetic traffic
//	llm-hops bench  [-to URL] [-rate 10000] [-seconds 10]                a load test
//	llm-hops vacuum [-db hops.db]                                        rebuild an older file so it shrinks after pruning (server stopped)
//	llm-hops config                                                      an example configuration file
//	llm-hops version
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/adapters/ollama"
	"github.com/YauhenBichel/llm-hops/internal/adapters/yllmgateway"
	"github.com/YauhenBichel/llm-hops/internal/config"
	"github.com/YauhenBichel/llm-hops/internal/demo"
	"github.com/YauhenBichel/llm-hops/internal/mcp"
	"github.com/YauhenBichel/llm-hops/internal/model"
	"github.com/YauhenBichel/llm-hops/internal/server"
	"github.com/YauhenBichel/llm-hops/internal/store"
)

var version = "dev" // set by the Makefile from the git tag

// versionString is the stamped version, or for a binary built without the Makefile (`go install
// github.com/YauhenBichel/llm-hops/cmd/llm-hops@v0.2.4`) the module version Go recorded in it.
func versionString() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := moduleVersion(bi.Main.Version); v != "" {
			return v
		}
	}
	return version
}

func moduleVersion(v string) string {
	if v == "" || v == "(devel)" {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

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
		err = importFile(os.Args[2:])
	case "export":
		err = export(os.Args[2:])
	case "demo":
		err = demoCmd(os.Args[2:])
	case "bench":
		err = bench(os.Args[2:])
	case "mcp":
		err = mcpCmd(os.Args[2:])
	case "vacuum":
		err = vacuum(os.Args[2:])
	case "config":
		fmt.Print(config.Example)
	case "version", "-v", "--version":
		fmt.Println("llm-hops", versionString())
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

  llm-hops serve  [-config hops.toml] [-db hops.db] [-listen 127.0.0.1:11602] [-tail FILE]... [-from-start] [-ollama-journal] [-demo N] [-live] [-keep-days 14]
  llm-hops tail   FILE [-to http://127.0.0.1:11602] [-from-start]
  llm-hops import FILE [-to http://127.0.0.1:11602]        a yllm-gateway request log, or an llm-hops export
  llm-hops export [-db hops.db] [-since MS] [-until MS]    every span as JSON lines, oldest first
  llm-hops demo   [-to http://127.0.0.1:11602] [-n 300] [-live]
  llm-hops bench  [-to http://127.0.0.1:11602] [-rate 10000] [-seconds 10]
  llm-hops mcp    [-to http://127.0.0.1:11602]             a Model Context Protocol server over stdio (Claude Code, Cursor, ...)
  llm-hops vacuum [-db hops.db]                            rebuild a file made by v0.2.4 or older so it shrinks after pruning; stop the server first
  llm-hops config                                          print an example configuration file
  llm-hops version

  A token (config "token" or LLM_HOPS_TOKEN on the server) is sent as -token, or LLM_HOPS_TOKEN, by the other commands.`)
}

// loadConfig reads -config, then lets the flags that were given on the command line win.
func loadConfig(fs *flag.FlagSet, args []string) (config.Config, error) {
	path := fs.String("config", "", "a TOML file (see: llm-hops config)")
	db := fs.String("db", "", "the SQLite file")
	listen := fs.String("listen", "", "address to listen on")
	var tails multi
	fs.Var(&tails, "tail", "follow a yllm-gateway request log (repeatable)")
	fromStart := fs.Bool("from-start", false, "read the whole log first, then follow")
	demoN := fs.Int("demo", 0, "load N synthetic traces first")
	live := fs.Bool("live", false, "with -demo: keep adding one trace a second")
	keepDays := fs.Float64("keep-days", 0, "spans older than this become daily statistics")
	title := fs.String("title", "", "the page's title")
	ollamaJournal := fs.Bool("ollama-journal", false, "follow journalctl -u ollama: every model load becomes a trace")
	maxSpans := fs.Int("max-spans-per-trace", 0, "spans one trace may hold; more are dropped and counted (0: no cap; default 5000)")
	maxDBMB := fs.Int64("max-db-mb", 0, "roll up and drop the oldest days when the data is larger than this (0: no limit)")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Load(*path, *path != "")
	if err != nil {
		return cfg, err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "db":
			cfg.DB = *db
		case "listen":
			cfg.Listen = *listen
		case "tail":
			cfg.Tail = tails
		case "from-start":
			cfg.FromStart = *fromStart
		case "demo":
			cfg.Demo = *demoN
		case "live":
			cfg.Live = *live
		case "keep-days":
			cfg.KeepDays = *keepDays
		case "title":
			cfg.Title = *title
		case "ollama-journal":
			cfg.OllamaJournal = *ollamaJournal
		case "max-spans-per-trace":
			cfg.MaxSpansPerTrace = *maxSpans
		case "max-db-mb":
			cfg.MaxDBMB = *maxDBMB
		}
	})
	if cfg.KeepDays <= 0 {
		return cfg, errors.New("keep-days must be positive")
	}
	if cfg.MaxSpansPerTrace < 0 || cfg.MaxDBMB < 0 {
		return cfg, errors.New("max-spans-per-trace and max-db-mb must not be negative")
	}
	return cfg, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	st.MaxSpansPerTrace = cfg.MaxSpansPerTrace
	if !st.Shrinks() {
		log.Printf("%s was made by v0.2.4 or older: pruning frees room inside the file but the file does not get smaller. "+
			"To change that once, stop the server and run: llm-hops vacuum -db %s", cfg.DB, cfg.DB)
	}
	srv := server.New(st, server.NewBroadcast())
	srv.Token = cfg.Token
	srv.Title = cfg.Title
	if cfg.Demo > 0 {
		g := demo.New(7, model.NowMS()-3_600_000, cfg.Demo, 3600)
		for i := 0; i < cfg.Demo; i++ {
			if _, err := st.Add(g.Next()); err != nil {
				return err
			}
		}
		log.Printf("demo: %d synthetic traces over the last hour", cfg.Demo)
	}
	stop := make(chan struct{})
	for _, path := range cfg.Tail {
		lines := make(chan string, 1000)
		go yllmgateway.Follow(path, cfg.FromStart, 500*time.Millisecond, stop, lines)
		go func(path string) {
			batch := []model.Span{}
			flush := func() {
				if len(batch) == 0 {
					return
				}
				touched, err := st.AddIfAbsent(batch) // the gateway's own spans, when it posts them, win
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
	if cfg.OllamaJournal {
		loads := make(chan []model.Span, 100)
		ctx, cancelJournal := context.WithCancel(context.Background())
		go func() {
			<-stop
			cancelJournal()
		}()
		go func() {
			if err := ollama.Follow(ctx, cfg.OllamaSince, &ollama.APINamer{Base: cfg.OllamaURL}, loads); err != nil && ctx.Err() == nil {
				log.Printf("ollama journal: %v", err)
			}
		}()
		go func() {
			for spans := range loads {
				if touched, err := st.Add(spans); err == nil {
					srv.Publish(touched)
				}
			}
		}()
		log.Printf("following journalctl -u ollama for model loads")
	}
	if cfg.Demo > 0 && cfg.Live {
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(1500 * time.Millisecond):
				}
				g := demo.New(time.Now().UnixNano(), model.NowMS()-5000, 1, 1)
				if touched, err := st.Add(g.Next()); err == nil {
					srv.Publish(touched)
				}
			}
		}()
	}
	// bind before anything is announced or started on the data: a taken port must fail here, not after
	// a line that says the server is up
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	go func() {
		for { // once at the start (a server restarted daily would never prune otherwise), then every ten minutes
			maintain(st, cfg)
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Minute):
			}
		}
	}()
	h := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	h.RegisterOnShutdown(srv.CloseStreams)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	guarded := ""
	if cfg.Token != "" {
		guarded = ", a token is required"
	}
	log.Printf("llm-hops %s on http://%s/  (db %s, keep %.0f days%s)", versionString(), ln.Addr(), cfg.DB, cfg.KeepDays, guarded)
	if cfg.Token == "" && !loopback(ln.Addr()) {
		log.Printf("warning: listening on %s without a token: anyone who can reach this address can read the traces and send spans. "+
			"Set token in the configuration or LLM_HOPS_TOKEN, or listen on 127.0.0.1", ln.Addr())
	}
	return serveUntil(ctx, h, ln, stop, shutdownGrace)
}

// loopback says whether the address can only be reached from this machine.
func loopback(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// maintain keeps the file within keep_days and max_db_mb, and hands the freed pages back to the disk.
func maintain(st *store.Store, cfg config.Config) {
	pruned := false
	if n, err := st.RollupAndPrune(int64(cfg.KeepDays*86_400_000), model.NowMS()); err != nil {
		log.Printf("rollup: %v", err)
	} else if n > 0 {
		pruned = true
		log.Printf("rolled up and pruned %d traces older than %.1f days", n, cfg.KeepDays)
	}
	if cfg.MaxDBMB > 0 {
		n, over, err := st.PruneToSize(cfg.MaxDBMB<<20, model.NowMS())
		if err != nil {
			log.Printf("max_db_mb: %v", err)
		}
		if n > 0 {
			pruned = true
			log.Printf("rolled up and pruned %d traces of the oldest days to stay under max_db_mb = %d", n, cfg.MaxDBMB)
		}
		if over {
			log.Printf("warning: today's traces alone are larger than max_db_mb = %d; a day in progress is never pruned", cfg.MaxDBMB)
		}
	}
	if !pruned {
		return
	}
	if freed, err := st.FreePages(); err != nil {
		log.Printf("freeing pages: %v", err)
	} else if freed > 0 {
		log.Printf("gave %d MB back to the disk", freed>>20)
	}
}

// shutdownGrace is how long requests in flight get to finish after a signal.
const shutdownGrace = 5 * time.Second

// serveUntil serves on ln until ctx ends, then closes stop and drains. Serve returns the moment
// Shutdown begins, so it waits for Shutdown as well: returning earlier would end the process with
// requests still running.
func serveUntil(ctx context.Context, h *http.Server, ln net.Listener, stop chan struct{}, grace time.Duration) error {
	drained := make(chan error, 1)
	go func() {
		<-ctx.Done()
		close(stop)
		shutdown, c := context.WithTimeout(context.Background(), grace)
		defer c()
		drained <- h.Shutdown(shutdown)
	}()
	if err := h.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if err := <-drained; err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// client is what the sending commands share: where, and with which token.
type client struct {
	to, token string
}

func clientFlags(fs *flag.FlagSet) *client {
	c := &client{}
	fs.StringVar(&c.to, "to", "http://127.0.0.1:11602", "the llm-hops server")
	fs.StringVar(&c.token, "token", os.Getenv("LLM_HOPS_TOKEN"), "the server's token, if it has one (or LLM_HOPS_TOKEN)")
	return c
}

func tail(args []string) error {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	c := clientFlags(fs)
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
				c.postIfAbsent(batch)
				batch = batch[:0]
			}
		case <-t.C:
			if len(batch) > 0 {
				c.postIfAbsent(batch)
				batch = batch[:0]
			}
		case <-stop:
			if len(batch) > 0 {
				c.postIfAbsent(batch)
			}
			return nil
		}
	}
}

// lineToSpans reads either format: a yllm-gateway request line, or an llm-hops export line (one span).
func lineToSpans(line string) []model.Span {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `"trace_id"`) {
		if sp, err := model.Clean(json.RawMessage(trimmed)); err == nil {
			return []model.Span{sp}
		}
		return nil
	}
	return yllmgateway.LineToSpans(line)
}

func importFile(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	c := clientFlags(fs)
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	batch := []model.Span{}
	n := 0
	for sc.Scan() {
		batch = append(batch, lineToSpans(sc.Text())...)
		if len(batch) >= 2000 {
			n += c.postIfAbsent(batch)
			batch = batch[:0]
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		n += c.postIfAbsent(batch)
	}
	log.Printf("imported %d traces", n)
	return nil
}

func export(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	db := fs.String("db", "hops.db", "the SQLite file")
	since := fs.Int64("since", 0, "milliseconds since the epoch")
	until := fs.Int64("until", 1<<62, "milliseconds since the epoch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	w := bufio.NewWriterSize(os.Stdout, 1<<20)
	defer w.Flush()
	enc := json.NewEncoder(w)
	n := 0
	err = st.ExportSpans(context.Background(), *since, *until, func(sp model.Span) error {
		n++
		return enc.Encode(sp)
	})
	if err != nil {
		return err
	}
	log.Printf("exported %d spans", n)
	return nil
}

func demoCmd(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	c := clientFlags(fs)
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
	c.post(spans)
	log.Printf("sent %d synthetic traces to %s", *n, c.to)
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
			c.post(g.Next())
		}
	}
}

// vacuum rebuilds a database file so that it gives room back to the disk after pruning from then on.
func vacuum(args []string) error {
	fs := flag.NewFlagSet("vacuum", flag.ExitOnError)
	db := fs.String("db", "hops.db", "the SQLite file; the server that uses it must be stopped")
	if err := fs.Parse(args); err != nil {
		return err
	}
	before, after, err := store.Vacuum(*db)
	if err != nil {
		return err
	}
	log.Printf("%s: %d MB before, %d MB now; from here on it shrinks after every prune", *db, before>>20, after>>20)
	return nil
}

// mcpCmd serves the Model Context Protocol over stdin and stdout, against a running llm-hops server.
func mcpCmd(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	c := clientFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	log.SetOutput(os.Stderr)
	s := &mcp.Server{Base: c.to, Token: c.token, Version: versionString()}
	return s.Serve(os.Stdin, os.Stdout)
}

// bench posts synthetic traces at a target rate of spans per second and reports what the server accepted.
func bench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	c := clientFlags(fs)
	rate := fs.Int("rate", 10000, "spans per second to send")
	seconds := fs.Int("seconds", 10, "how long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	g := demo.New(time.Now().UnixNano(), model.NowMS(), *rate**seconds, float64(*seconds))
	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
	var sent, accepted, posts, failed int
	var slowest time.Duration
	start := time.Now()
	for time.Now().Before(deadline) {
		tick := time.Now()
		batch := []model.Span{}
		for len(batch) < *rate/10 { // ten posts a second
			batch = append(batch, g.Next()...)
		}
		t0 := time.Now()
		got := c.post(batch)
		d := time.Since(t0)
		if d > slowest {
			slowest = d
		}
		posts++
		sent += len(batch)
		if got == 0 {
			failed++
		} else {
			accepted += len(batch)
		}
		if wait := 100*time.Millisecond - time.Since(tick); wait > 0 {
			time.Sleep(wait)
		}
	}
	el := time.Since(start)
	fmt.Printf("bench: %d spans in %d posts over %.1f s (%.0f spans/s); %d accepted, %d posts failed; slowest post %s\n",
		sent, posts, el.Seconds(), float64(sent)/el.Seconds(), accepted, failed, slowest.Round(time.Millisecond))
	return nil
}

// post sends spans and returns the number of traces the server reports; it retries twice.
func (c *client) post(spans []model.Span) int { return c.send(spans, false) }

// postIfAbsent is post for log-derived spans: the server keeps what a service already posted itself.
func (c *client) postIfAbsent(spans []model.Span) int { return c.send(spans, true) }

func (c *client) send(spans []model.Span, ifAbsent bool) int {
	var body []byte
	if ifAbsent {
		body, _ = json.Marshal(map[string]any{"spans": spans, "if_absent": true})
	} else {
		body, _ = json.Marshal(spans)
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, strings.TrimRight(c.to, "/")+"/api/v1/spans", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := http.DefaultClient.Do(req)
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
			log.Printf("llm-hops: could not post %d spans to %s: %v", len(spans), c.to, err)
			return 0
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return 0
}

var _ = strconv.Itoa
