package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("answer is not JSON (%q): %v", rec.Body.String(), err)
	}
	return out
}

// A failed swap must say why. Xirp 0.22.0 refuses to swap to an agent CLI older than
// the version it was tested against (code UNSUPPORTED_AGENT_VERSION, new in 0.22.0),
// and the reason only exists in the typed error frame.
func TestSwapAgentReportsTheDaemonsReason(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		if req["type"] == "session:swap-agent" {
			send(map[string]any{
				"type":      "session:swap-agent:error",
				"sessionId": "s1",
				"code":      "UNSUPPORTED_AGENT_VERSION",
				"message":   "cursor 2026.01.01 is older than the tested minimum",
				"hint":      "update it: curl https://cursor.com/install -fsS | bash",
			})
		}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/s1/swap", strings.NewReader(`{"agent":"cursor"}`))
	start := time.Now()
	sessionSwapAgent(rec, req, "s1")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %v: the typed error was skipped and the handler waited out its timeout", took)
	}
	if rec.Code != 502 {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"UNSUPPORTED_AGENT_VERSION", "older than the tested minimum", "cursor.com/install"} {
		if !strings.Contains(body, want) {
			t.Fatalf("answer %q does not contain %q", body, want)
		}
	}
}

// session:agent-swapped carries sessionId, from and to, and no session object, so the
// handler has to read the row back or the phone gets an empty session.
func TestSwapAgentReturnsTheUpdatedSession(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		switch req["type"] {
		case "session:swap-agent":
			send(map[string]any{"type": "session:updated", "session": map[string]any{"id": "s1"}})
			send(map[string]any{"type": "session:agent-swapped", "sessionId": "s1", "from": "claude", "to": "codex"})
		case "session:get":
			send(map[string]any{"type": "session:get", "session": map[string]any{
				"id": "s1", "name": "Fix the flake", "status": "running", "currentAgent": "codex",
			}})
		case "projects:list":
			send(map[string]any{"type": "projects:list", "projects": []any{}})
		}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/s1/swap", strings.NewReader(`{"agent":"codex"}`))
	sessionSwapAgent(rec, req, "s1")
	if rec.Code != 200 {
		t.Fatalf("status %d (%s), want 200", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	sm, _ := out["session"].(map[string]any)
	if got, _ := sm["currentAgent"].(string); got != "codex" {
		t.Fatalf("session.currentAgent = %q, want codex; whole answer: %s", got, rec.Body.String())
	}
	if got, _ := out["from"].(string); got != "claude" {
		t.Fatalf("from = %q, want claude", got)
	}
}

// Three sessions carry a tmuxSession while tmux reports no live pane. That is the
// shape that made the list force a fresh tmux read per session, two serialized daemon
// calls each, on every five-second poll.
func TestSessionListForcesAtMostOneTmuxRefresh(t *testing.T) {
	f := newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		switch req["type"] {
		case "sessions:list":
			send(map[string]any{"type": "sessions:list", "sessions": []any{
				map[string]any{"id": "a", "status": "running", "tmuxSession": "xirp-a"},
				map[string]any{"id": "b", "status": "running", "tmuxSession": "xirp-b"},
				map[string]any{"id": "c", "status": "running", "tmuxSession": "xirp-c"},
			}})
		case "projects:list":
			send(map[string]any{"type": "projects:list", "projects": []any{}})
		case "tmux:status":
			send(map[string]any{"type": "tmux:status", "available": true, "sessions": []any{}})
		case "tmux:orphaned-sessions":
			send(map[string]any{"type": "tmux:orphaned-sessions", "sessions": []any{}})
		case "modules:list":
			send(map[string]any{"type": "modules:list", "modules": []any{}})
		}
	})
	rec := httptest.NewRecorder()
	handleSessions(rec, httptest.NewRequest("GET", "/api/sessions", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d (%s)", rec.Code, rec.Body.String())
	}
	// One cold read, then at most one forced re-read for the whole request.
	if got := f.countOf("tmux:status"); got > 2 {
		t.Fatalf("tmux:status called %d times for 3 paneless sessions, want at most 2", got)
	}
	out := decode(t, rec)
	list, _ := out["sessions"].([]any)
	if len(list) != 3 {
		t.Fatalf("sessions: got %d, want 3", len(list))
	}
	for _, s := range list {
		sm := s.(map[string]any)
		if has, ok := sm["hasPane"].(bool); !ok || has {
			t.Fatalf("session %v: hasPane = %v, want false", sm["id"], sm["hasPane"])
		}
	}
}

// The files module reports a failed read inside the success frame:
// {type:"files:read", path, error}. Read as a success it renders an empty file.
func TestSessionFileReportsWhyAReadFailed(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		switch req["type"] {
		case "session:get":
			send(map[string]any{"type": "session:get", "session": map[string]any{
				"id": "s1", "projectId": "p1", "worktreePath": "/tmp/wt",
			}})
		case "files:stat":
			send(map[string]any{"type": "files:stat", "path": "src/gone.go", "exists": false})
		case "files:read":
			send(map[string]any{
				"type": "files:read", "path": "src/gone.go",
				"error": "ENOENT: no such file or directory, open '/tmp/wt/src/gone.go'",
			})
		}
	})
	rec := httptest.NewRecorder()
	sessionFile(rec, httptest.NewRequest("GET", "/api/sessions/s1/file?path=src/gone.go", nil), "s1")
	out := decode(t, rec)
	why, _ := out["unavailable"].(string)
	if why == "" {
		t.Fatalf("no `unavailable` in the answer, so a missing file reads as an empty one: %s", rec.Body.String())
	}
	if !strings.Contains(why, "no such file") {
		t.Fatalf("unavailable = %q, want the daemon's own reason", why)
	}
}

// A directory is not a file, and the daemon's read error for one is not obvious. With
// files:stat, new in 0.20.1, the answer can say which it is.
func TestSessionFileNamesADirectory(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		switch req["type"] {
		case "session:get":
			send(map[string]any{"type": "session:get", "session": map[string]any{
				"id": "s1", "projectId": "p1",
			}})
		case "files:stat":
			send(map[string]any{
				"type": "files:stat", "path": "web",
				"exists": true, "isFile": false, "isDirectory": true,
			})
		}
	})
	rec := httptest.NewRecorder()
	sessionFile(rec, httptest.NewRequest("GET", "/api/sessions/s1/file?path=web", nil), "s1")
	out := decode(t, rec)
	if why, _ := out["unavailable"].(string); !strings.Contains(why, "directory") {
		t.Fatalf("unavailable = %q, want it to name the directory: %s", why, rec.Body.String())
	}
}

// The daemon answers nothing at all for a permission request that has expired
// (ws/handlers/chirp-settings.js logs a debug line and returns), and it only holds one
// for about 500 ms, so expired is the normal case from a phone. Waiting 15 seconds to
// then say "timeout" is the worst of both.
func TestPermissionRespondFailsFastWhenTheRequestExpired(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		// Deliberately silent.
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/permissions/perm-1", strings.NewReader(`{"behavior":"allow"}`))
	start := time.Now()
	handlePermissionRespond(rec, req)
	took := time.Since(start)
	if took > 6*time.Second {
		t.Fatalf("took %v: a phone taps this and waits", took)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "expired") {
		t.Fatalf("answer %q does not say the request had expired", body)
	}
}

// The terminal view renders the pane, not the transcript, so its four-second poll must
// not spawn `node squab session-parse` for a transcript nothing displays.
func TestSessionDetailCanSkipTheTranscript(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		switch req["type"] {
		case "session:get":
			send(map[string]any{"type": "session:get", "session": map[string]any{
				"id": "s1", "name": "Fix the flake", "status": "running",
			}})
		case "projects:list":
			send(map[string]any{"type": "projects:list", "projects": []any{}})
		}
	})
	rec := httptest.NewRecorder()
	sessionDetail(rec, httptest.NewRequest("GET", "/api/sessions/s1?transcript=0", nil), "s1")
	out := decode(t, rec)
	if _, ok := out["session"]; !ok {
		t.Fatalf("no session in the answer: %s", rec.Body.String())
	}
	if _, ok := out["messages"]; ok {
		t.Fatal("messages present: the transcript was read for a view that does not show it")
	}
	if _, ok := out["transcriptError"]; ok {
		t.Fatal("transcriptError present: squab was run anyway")
	}
}

// The push watcher's goroutine reads projectsCached while HTTP handlers write it. Run
// under -race, this is the test that says so.
func TestProjectsCacheIsSafeForConcurrentUse(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		if req["type"] == "projects:list" {
			send(map[string]any{"type": "projects:list", "projects": []any{
				map[string]any{"id": "p1", "name": "webapp", "defaultBranch": "main"},
			}})
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := projectNames()["p1"]; got != "" && got != "webapp" {
				t.Errorf("project name = %q", got)
			}
			if got := projectBase("p1"); got != "" && got != "main" {
				t.Errorf("project base = %q", got)
			}
		}()
	}
	wg.Wait()

	// The base branch moved into the same cache as the names, and the changes screen
	// compares against it. An empty one silently drops the branch diff.
	if got := projectBase("p1"); got != "main" {
		t.Fatalf("projectBase = %q, want main", got)
	}
	if got := projectBase("nope"); got != "" {
		t.Fatalf("projectBase for an unknown project = %q, want empty", got)
	}
}

// files:read has no offset or limit, so the daemon loads and sends the whole file before
// anything here can cut it. A bundle is refused from the stat instead.
func TestSessionFileRefusesSomethingTooBigToRead(t *testing.T) {
	f := newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		switch req["type"] {
		case "session:get":
			send(map[string]any{"type": "session:get", "session": map[string]any{
				"id": "s1", "projectId": "p1",
			}})
		case "files:stat":
			send(map[string]any{
				"type": "files:stat", "path": "web/bundle.js",
				"exists": true, "isFile": true, "isDirectory": false,
				"size": float64(fileReadCap + 1),
			})
		}
	})
	rec := httptest.NewRecorder()
	sessionFile(rec, httptest.NewRequest("GET", "/api/sessions/s1/file?path=web/bundle.js", nil), "s1")
	out := decode(t, rec)
	why, _ := out["unavailable"].(string)
	for _, want := range []string{"5.0 MB", "5 MB"} {
		if !strings.Contains(why, want) {
			t.Fatalf("unavailable = %q, want it to name %q", why, want)
		}
	}
	if n := f.countOf("files:read"); n != 0 {
		t.Fatalf("files:read was sent %d times for a file over the cap", n)
	}
}
