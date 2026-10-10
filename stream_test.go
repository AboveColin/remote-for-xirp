package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The stream has to say what changed and keep saying it, so the phone can stop polling.
func TestEventStreamCarriesChanges(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {})
	srv := httptest.NewServer(http.HandlerFunc(handleEvents))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	// A proxy that buffers the stream makes it look broken, so the header that stops
	// that is part of the contract.
	if got := res.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}

	lines := bufio.NewScanner(res.Body)
	// The handler opens with a retry hint, which also proves it flushed.
	deadline := time.Now().Add(3 * time.Second)
	sawRetry := false
	for !sawRetry && time.Now().Before(deadline) {
		if !lines.Scan() {
			t.Fatal("stream closed before the retry hint")
		}
		sawRetry = strings.HasPrefix(lines.Text(), "retry:")
	}
	if !sawRetry {
		t.Fatal("no retry hint")
	}

	// Wait for the subscription to land, then publish.
	for i := 0; i < 100; i++ {
		live.mu.RLock()
		n := len(live.subs)
		live.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	live.publish(change{Kind: "session", ID: "s1"})

	for time.Now().Before(deadline) {
		if !lines.Scan() {
			t.Fatal("stream closed before the change arrived")
		}
		line := lines.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if want := `data: {"kind":"session","id":"s1"}`; line != want {
			t.Fatalf("frame %q, want %q", line, want)
		}
		return
	}
	t.Fatal("the change never arrived")
}

// A refusal has to name the limit, or a phone that cannot connect learns nothing.
func TestEventStreamCapNamesTheLimit(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {})
	srv := httptest.NewServer(http.HandlerFunc(handleEvents))
	defer srv.Close()

	var bodies []*http.Response
	defer func() {
		for _, res := range bodies {
			res.Body.Close()
		}
	}()
	for i := 0; i < maxStreams; i++ {
		res, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		bodies = append(bodies, res)
	}
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("the one over the limit: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatalf("status %d for the stream over the limit, want 503", res.StatusCode)
	}
	body := make([]byte, 256)
	n, _ := res.Body.Read(body)
	if !strings.Contains(string(body[:n]), "8 event streams") {
		t.Fatalf("refusal %q does not name the limit", body[:n])
	}
}

// Every stream must let go of its subscription, or the store publishes to the dead
// forever.
func TestClosingAStreamReleasesItsSubscription(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {})
	srv := httptest.NewServer(http.HandlerFunc(handleEvents))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	for i := 0; i < 100; i++ {
		live.mu.RLock()
		n := len(live.subs)
		live.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	res.Body.Close()

	for i := 0; i < 200; i++ {
		live.publish(change{Kind: "sessions"})
		live.mu.RLock()
		n := len(live.subs)
		live.mu.RUnlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the subscription outlived the stream")
}
