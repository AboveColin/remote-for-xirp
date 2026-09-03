package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeDaemon stands in for the Xirp daemon's WebSocket API, so these tests can exercise
// the client's framing, reply matching and error handling without the app installed.
// Each test scripts its own reply function.
type fakeDaemon struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string
}

// reply answers one request. It runs on that connection's own read goroutine, so
// sleeping in it delays the answer, which is how a reply that arrives after the client
// gave up is produced.
type reply func(req map[string]any, send func(map[string]any))

func newFakeDaemon(t *testing.T, r reply) *fakeDaemon {
	t.Helper()
	f := &fakeDaemon{}
	up := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, hr *http.Request) {
		conn, err := up.Upgrade(w, hr, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var wmu sync.Mutex
		send := func(frame map[string]any) {
			wmu.Lock()
			defer wmu.Unlock()
			_ = conn.WriteJSON(frame)
		}
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if json.Unmarshal(raw, &msg) != nil {
				continue
			}
			typ, _ := msg["type"].(string)
			f.mu.Lock()
			f.calls = append(f.calls, typ)
			f.mu.Unlock()
			r(msg, send)
		}
	}))

	_, port, err := net.SplitHostPort(f.srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("fake daemon address: %v", err)
	}
	prevDiscover := discoverCreds
	discoverCreds = func() (Creds, error) { return Creds{Port: port, Token: "test"}, nil }
	prevClient := client
	client = NewClient()
	resetCaches()
	t.Cleanup(func() {
		client.mu.Lock()
		client.drop()
		client.mu.Unlock()
		client = prevClient
		discoverCreds = prevDiscover
		f.srv.Close()
		resetCaches()
	})
	return f
}

// countOf reports how many times the fake was asked for one request type.
func (f *fakeDaemon) countOf(typ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == typ {
			n++
		}
	}
	return n
}

// resetCaches empties everything the handlers memoise, so one test's daemon answers
// cannot be served to the next.
func resetCaches() {
	tmuxMu.Lock()
	tmuxCache = tmuxState{}
	tmuxMu.Unlock()
	modulesMu.Lock()
	modulesList, modulesAt = nil, time.Time{}
	modulesMu.Unlock()
	projectMu.Lock()
	pcache = projectCache{}
	projectMu.Unlock()
	metaMu.Lock()
	mcache = metaCache{}
	metaMu.Unlock()
}

func TestCallReturnsWantedReplyAndSkipsBroadcasts(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		// The daemon pushes unrelated traffic to every connected client: its database
		// debug feed emits a frame per query. None of it may satisfy a call.
		send(map[string]any{"type": "db:query", "record": map[string]any{"id": 1}})
		send(map[string]any{"type": "session:updated", "session": map[string]any{"id": "other"}})
		send(map[string]any{"type": "sessions:list", "sessions": []any{map[string]any{"id": "s1"}}})
	})
	res, err := client.Call(map[string]any{"type": "sessions:list"}, "sessions:list", 3*time.Second)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if list, _ := res["sessions"].([]any); len(list) != 1 {
		t.Fatalf("sessions: got %d, want 1", len(list))
	}
}

// A rejected request comes back as a generic error frame naming the request it
// answers, which every call already surfaces.
func TestCallSurfacesGenericError(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		send(map[string]any{
			"type":         "error",
			"originalType": "session:create",
			"message":      `Coding agent "cursor" is not installed.`,
		})
	})
	_, err := client.Call(map[string]any{"type": "session:create"}, "session:created", 3*time.Second)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("error %q drops the daemon's message", err)
	}
}

// git:status and git:log answer git:error, not the generic error frame, when the
// worktree directory is gone. Receipt, Xirp 0.22.0 ws/helpers.js
// resolveGitCwdOrSendError: it sends
// {type:"git:error",code:"DIRECTORY_MISSING",message:"Directory does not exist: <path>"}.
func TestCallSurfacesGitError(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		send(map[string]any{
			"type":    "git:error",
			"code":    "DIRECTORY_MISSING",
			"message": "Directory does not exist: /tmp/gone",
		})
	})
	start := time.Now()
	_, err := client.Call(map[string]any{"type": "git:status", "projectId": "p"}, "git:status", 4*time.Second, "git:error")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %v: the error frame was skipped and the call waited for its timeout", took)
	}
	for _, want := range []string{"DIRECTORY_MISSING", "/tmp/gone"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

// A failed agent swap reports its reason as session:swap-agent:error. Receipt, Xirp
// 0.22.0 ws/handlers/agents.js: every failure path sends that type with a code, a
// message and sometimes a hint. UNSUPPORTED_AGENT_VERSION is new in 0.22.0.
func TestCallSurfacesSwapAgentError(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		send(map[string]any{
			"type":      "session:swap-agent:error",
			"sessionId": "s1",
			"code":      "UNSUPPORTED_AGENT_VERSION",
			"message":   "cursor 2026.01.01 is older than the tested minimum",
			"hint":      "run: cursor upgrade",
		})
	})
	start := time.Now()
	_, err := client.Call(
		map[string]any{"type": "session:swap-agent", "sessionId": "s1", "agentName": "cursor"},
		"session:agent-swapped", 4*time.Second, "session:swap-agent:error",
	)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %v: the typed error was skipped and the call waited for its timeout", took)
	}
	for _, want := range []string{"UNSUPPORTED_AGENT_VERSION", "older than the tested minimum", "cursor upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

// A reply that arrives after its call gave up must not be handed to the next call
// waiting for that same type. Without dropping the connection on timeout it sits in
// the socket buffer and the next caller reads it as its own answer.
func TestCallTimeoutDoesNotLeakItsReplyToTheNextCall(t *testing.T) {
	var n int
	var mu sync.Mutex
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		mu.Lock()
		n++
		which := n
		mu.Unlock()
		if which == 1 {
			// Later than the first call's patience, sooner than the second's.
			time.Sleep(900 * time.Millisecond)
			send(map[string]any{"type": "session:get", "session": map[string]any{"id": "stale"}})
			return
		}
		send(map[string]any{"type": "session:get", "session": map[string]any{"id": "fresh"}})
	})

	if _, err := client.Call(map[string]any{"type": "session:get", "sessionId": "stale"}, "session:get", 300*time.Millisecond); err == nil {
		t.Fatal("first call: want a timeout, got nil")
	}
	res, err := client.Call(map[string]any{"type": "session:get", "sessionId": "fresh"}, "session:get", 4*time.Second)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	sm, _ := res["session"].(map[string]any)
	if id, _ := sm["id"].(string); id != "fresh" {
		t.Fatalf("second call got the first call's reply: session id %q", id)
	}
}

// A timed-out request must not be sent again. The daemon may already be acting on it,
// so a retry of session:create would create a second session.
func TestCallDoesNotResendAfterTimeout(t *testing.T) {
	f := newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		// Never answers.
	})
	if _, err := client.Call(map[string]any{"type": "session:create"}, "session:created", 250*time.Millisecond); err == nil {
		t.Fatal("want a timeout, got nil")
	}
	if got := f.countOf("session:create"); got != 1 {
		t.Fatalf("session:create sent %d times, want 1", got)
	}
}

// A git:error left unread by a timed-out call must not surface as the next git call's
// failure. The connection is dropped on timeout for this reason; without that the frame
// waits in the socket buffer for whoever reads next.
func TestGitErrorCannotLeakIntoTheNextCall(t *testing.T) {
	var n int
	var mu sync.Mutex
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		mu.Lock()
		n++
		which := n
		mu.Unlock()
		if which == 1 {
			time.Sleep(900 * time.Millisecond)
			send(map[string]any{
				"type": "git:error", "code": "DIRECTORY_MISSING",
				"message": "Directory does not exist: /tmp/gone",
			})
			return
		}
		send(map[string]any{"type": "git:status", "status": map[string]any{"branch": "main", "files": []any{}}})
	})

	if _, err := client.Call(map[string]any{"type": "git:status"}, "git:status", 300*time.Millisecond, "git:error"); err == nil {
		t.Fatal("first call: want a timeout, got nil")
	}
	res, err := client.Call(map[string]any{"type": "git:status"}, "git:status", 4*time.Second, "git:error")
	if err != nil {
		t.Fatalf("second call inherited the first call's error: %v", err)
	}
	st, _ := res["status"].(map[string]any)
	if branch, _ := st["branch"].(string); branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
}

// The daemon broadcasts session:updated for every session's every status change, so a
// reply matched on type alone can be about someone else's session. Stopping A then
// reported B's status.
func TestCallIgnoresAnotherSessionsUpdate(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		// B changes status while A's stop is in flight.
		send(map[string]any{"type": "session:updated", "session": map[string]any{
			"id": "B", "status": "running",
		}})
		send(map[string]any{"type": "session:updated", "session": map[string]any{
			"id": "A", "status": "completed",
		}})
	})
	res, err := client.Call(map[string]any{"type": "session:stop", "sessionId": "A"}, "session:updated", 3*time.Second)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	sm, _ := res["session"].(map[string]any)
	if id, _ := sm["id"].(string); id != "A" {
		t.Fatalf("answered about session %q, want A", id)
	}
	if got, _ := sm["status"].(string); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}
}

// session:urls is broadcast too, so the same guard has to cover it.
func TestCallIgnoresAnotherSessionsUrls(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		send(map[string]any{"type": "session:urls", "sessionId": "B", "urls": []any{"http://b"}})
		send(map[string]any{"type": "session:urls", "sessionId": "A", "urls": []any{"http://a"}})
	})
	res, err := client.Call(map[string]any{"type": "session:urls:get", "sessionId": "A"}, "session:urls", 3*time.Second)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if urls, _ := res["urls"].([]any); len(urls) != 1 || urls[0] != "http://a" {
		t.Fatalf("urls = %v, want A's", res["urls"])
	}
}

// A fork asks about the source session and is answered with the copy, so session:created
// must not be held to the id in the request.
func TestCallAcceptsTheForksNewSession(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		send(map[string]any{"type": "session:created", "session": map[string]any{"id": "copy"}})
	})
	res, err := client.Call(
		map[string]any{"type": "session:fork", "sessionId": "source", "cliSessionId": "cli"},
		"session:created", 3*time.Second,
	)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	sm, _ := res["session"].(map[string]any)
	if id, _ := sm["id"].(string); id != "copy" {
		t.Fatalf("fork answered with %q, want the new session", id)
	}
}
