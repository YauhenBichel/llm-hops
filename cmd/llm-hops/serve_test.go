// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
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
	go func() { result <- serveUntil(ctx, h, stop, 5*time.Second) }()

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
	go func() { result <- serveUntil(ctx, h, make(chan struct{}), 50*time.Millisecond) }()
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
