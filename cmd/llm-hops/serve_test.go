// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YauhenBichel/llm-hops/internal/config"
	"github.com/YauhenBichel/llm-hops/internal/demo"
	"github.com/YauhenBichel/llm-hops/internal/model"
	"github.com/YauhenBichel/llm-hops/internal/store"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func listen(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// A signal must not end the process while a request is still running.
func TestServeUntilWaitsForRequestsInFlight(t *testing.T) {
	started := make(chan struct{})
	var finished atomic.Bool
	h := &http.Server{Addr: freeAddr(t), Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		finished.Store(true)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	result := make(chan error, 1)
	go func() { result <- serveUntil(ctx, h, listen(t, h.Addr), stop, 5*time.Second) }()

	var resp *http.Response
	var err error
	got := make(chan struct{})
	go func() {
		defer close(got)
		for i := 0; i < 50; i++ {
			if resp, err = http.Get("http://" + h.Addr); err == nil {
				resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	<-started
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("serveUntil: %v", err)
	}
	if !finished.Load() {
		t.Fatal("serveUntil returned with the request still running")
	}
	<-got
	if err != nil {
		t.Fatalf("the client's request failed: %v", err)
	}
	select {
	case <-stop:
	default:
		t.Fatal("stop was not closed")
	}
}

// A handler that outlasts the grace period is reported, not waited for forever.
func TestServeUntilReportsAnOverrun(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h := &http.Server{Addr: freeAddr(t), Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- serveUntil(ctx, h, listen(t, h.Addr), make(chan struct{}), 50*time.Millisecond) }()
	go func() {
		for i := 0; i < 50; i++ {
			if resp, err := http.Get("http://" + h.Addr); err == nil {
				resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("want an error when the grace period runs out")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveUntil did not return")
	}
}

// A taken port must be an error before anything says the server is up (the banner used to come first).
func TestServeOnATakenPortFailsBeforeTheBanner(t *testing.T) {
	taken := listen(t, "127.0.0.1:0")
	defer taken.Close()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	cfg := filepath.Join(t.TempDir(), "hops.toml")
	os.WriteFile(cfg, nil, 0o600)
	err := serve([]string{"-config", cfg, "-db", filepath.Join(t.TempDir(), "hops.db"), "-listen", taken.Addr().String()})
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(logged.String(), " on http://") {
		t.Fatalf("announced a server that never listened:\n%s", logged.String())
	}
}

func TestServeRefusesADamagedFileWithAReason(t *testing.T) {
	db := filepath.Join(t.TempDir(), "hops.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	g := demo.New(1, model.NowMS()-60_000, 200, 60)
	for i := 0; i < 200; i++ {
		st.Add(g.Next())
	}
	st.Close()
	for _, f := range []string{db + "-wal", db + "-shm"} {
		os.Remove(f)
	}
	fi, _ := os.Stat(db)
	if err := os.Truncate(db, fi.Size()/2); err != nil { // a copy that was cut short
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "hops.toml")
	os.WriteFile(cfg, nil, 0o600)
	err = serve([]string{"-config", cfg, "-db", db, "-listen", "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "is not a usable llm-hops database") {
		t.Fatalf("a file cut in half was served: err=%v", err)
	}
}

// The first prune comes at the start, not ten minutes later: a server restarted every few minutes pruned never.
func TestMaintainPrunesOldTracesAndKeepsWithinTheSize(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := model.NowMS()
	day := int64(86_400_000)
	var spans []model.Span
	for d := int64(0); d < 6; d++ { // one trace a day for six days, the newest today
		spans = append(spans, model.Span{TraceID: "d" + string(rune('0'+d)), SpanID: "r", Service: "gateway", Name: "request",
			StartMS: now - d*day - 1000, EndMS: now - d*day, Status: "ok", Attrs: model.Attrs{"pad": strings.Repeat("x", 500)}})
	}
	if _, err := st.Add(spans); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	left := func() int {
		n, err := st.CountTraces(ctx, store.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	cfg := config.Default()
	cfg.KeepDays = 3.5
	maintain(st, cfg)
	if n := left(); n != 4 {
		t.Fatalf("keep_days=3.5 left %d traces, not the 4 newest", n)
	}
	cfg.MaxDBMB = 1 // the data fits: nothing more goes
	maintain(st, cfg)
	if n := left(); n != 4 {
		t.Fatalf("max_db_mb=1 on a small file left %d", n)
	}
	daily, err := st.Daily(ctx, 0, now+day)
	if err != nil || len(daily) == 0 {
		t.Fatalf("the pruned days left no daily statistics: %v %v", daily, err)
	}
}

func TestOnlyLoopbackIsQuietWithoutAToken(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "0.0.0.0:1": false, "192.168.1.5:1": false, "[::]:1": false} {
		a, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if loopback(a) != want {
			t.Fatalf("%s: loopback=%v", addr, !want)
		}
	}
}

func TestTheVersionFallsBackToTheModuleVersion(t *testing.T) {
	for in, want := range map[string]string{"v0.2.4": "0.2.4", "(devel)": "", "": "", "v0.3.0-rc.1": "0.3.0-rc.1"} {
		if got := moduleVersion(in); got != want {
			t.Fatalf("%q: %q", in, got)
		}
	}
	if old := version; true {
		version = "1.2.3"
		if versionString() != "1.2.3" {
			t.Fatal("a stamped version must win")
		}
		version = old
	}
	if versionString() == "" {
		t.Fatal("no version at all")
	}
}
