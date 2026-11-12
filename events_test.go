package main

import (
	"testing"
	"time"
)

// coldStore puts the store back to the state it has at start-up, so fill() runs.
func coldStore() {
	live.mu.Lock()
	live.sessions = map[string]map[string]any{}
	live.projects = map[string]map[string]any{}
	live.permissions = map[string]map[string]any{}
	live.restorable = nil
	live.syncedAt = time.Time{}
	live.triedAt = time.Time{}
	live.drift = 0
	live.mu.Unlock()
}

// listAnswers is a fake daemon that answers the three calls a resync makes.
func listAnswers(sessions, projects, restorable []any) reply {
	return func(req map[string]any, send func(map[string]any)) {
		switch req["type"] {
		case "sessions:list":
			send(map[string]any{"type": "sessions:list", "sessions": sessions})
		case "projects:list":
			send(map[string]any{"type": "projects:list", "projects": projects})
		case "sessions:restorable":
			send(map[string]any{"type": "sessions:restorable", "sessions": restorable})
		}
	}
}

// The first read after a start has nothing to read, so it asks. Every read after that
// is answered from memory: that is the whole point, and the call count is the receipt.
func TestStoreFillsItselfOnceOnFirstRead(t *testing.T) {
	f := newFakeDaemon(t, listAnswers(
		[]any{map[string]any{"id": "s1", "status": "running", "projectId": "p1"}},
		[]any{map[string]any{"id": "p1", "name": "webapp", "defaultBranch": "main"}},
		nil,
	))
	coldStore()

	if got := len(live.sessionRows()); got != 1 {
		t.Fatalf("sessions after the first read: %d, want 1", got)
	}
	for i := 0; i < 20; i++ {
		live.sessionRows()
		live.projectRows()
		live.session("s1")
	}
	if got := f.countOf("sessions:list"); got != 1 {
		t.Fatalf("sessions:list asked %d times for 23 reads, want 1", got)
	}
	if got := projectNames()["p1"]; got != "webapp" {
		t.Fatalf("project name = %q", got)
	}
	if got := projectBase("p1"); got != "main" {
		t.Fatalf("project base = %q", got)
	}
}

// A daemon that answers nothing must not make every read wait out three call timeouts
// one after another.
func TestColdStoreDoesNotRetryOnEveryRead(t *testing.T) {
	f := newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {})
	coldStore()
	// Each attempt is three calls, and each call fails as fast as the fake lets it.
	// What matters is that ten reads do not make ten attempts.
	live.mu.Lock()
	live.triedAt = time.Now()
	live.mu.Unlock()
	for i := 0; i < 10; i++ {
		live.sessionRows()
	}
	if got := f.countOf("sessions:list"); got != 0 {
		t.Fatalf("a cold store asked %d times inside the retry window, want 0", got)
	}
}

func TestStoreAppliesSessionBroadcasts(t *testing.T) {
	coldStore()
	warmStore()

	if c, ok := live.apply(map[string]any{"type": "session:created", "session": map[string]any{
		"id": "s1", "status": "running",
	}}); !ok || c.Kind != "session" || c.ID != "s1" {
		t.Fatalf("created: change %+v ok=%v", c, ok)
	}
	if got := live.session("s1")["status"]; got != "running" {
		t.Fatalf("status after create = %v", got)
	}

	live.apply(map[string]any{"type": "session:updated", "session": map[string]any{
		"id": "s1", "status": "finished",
	}})
	if got := live.session("s1")["status"]; got != "finished" {
		t.Fatalf("status after update = %v, want finished", got)
	}

	if c, ok := live.apply(map[string]any{"type": "session:deleted", "sessionId": "s1"}); !ok || c.Kind != "sessions" {
		t.Fatalf("deleted: change %+v ok=%v", c, ok)
	}
	if live.session("s1") != nil {
		t.Fatal("the session survived session:deleted")
	}

	// A frame with no session id changes nothing rather than storing a row under "".
	if _, ok := live.apply(map[string]any{"type": "session:updated", "session": map[string]any{}}); ok {
		t.Fatal("a session:updated with no id was applied")
	}
}

func TestStoreAppliesProjectAndRestorableBroadcasts(t *testing.T) {
	coldStore()
	warmStore()

	live.apply(map[string]any{"type": "project:added", "project": map[string]any{
		"id": "p1", "name": "webapp", "defaultBranch": "main",
	}})
	if got := projectNames()["p1"]; got != "webapp" {
		t.Fatalf("project name = %q", got)
	}
	live.apply(map[string]any{"type": "project:updated", "project": map[string]any{
		"id": "p1", "name": "renamed", "defaultBranch": "trunk",
	}})
	if got := projectBase("p1"); got != "trunk" {
		t.Fatalf("base after update = %q", got)
	}
	live.apply(map[string]any{"type": "project:removed", "projectId": "p1"})
	if got := len(live.projectRows()); got != 0 {
		t.Fatalf("%d projects after removal, want 0", got)
	}

	live.apply(map[string]any{"type": "sessions:restorable", "sessions": []any{
		map[string]any{"id": "s1"}, map[string]any{"id": "s2"},
	}})
	if got := len(live.restorableRows()); got != 2 {
		t.Fatalf("%d restorable, want 2", got)
	}
}

// A permission request is answerable for about 500 ms, and the phone needs to know
// which side of that it is on: before, the daemon still holds it; after, only the
// agent's own dialog can answer.
func TestPermissionRequestsCarryWhetherTheyExpired(t *testing.T) {
	coldStore()
	warmStore()

	live.apply(map[string]any{
		"type": "permission:request", "requestId": "perm-1",
		"sessionId": "s1", "toolName": "Bash",
	})
	rows := live.permissionRows()
	if len(rows) != 1 {
		t.Fatalf("%d requests, want 1", len(rows))
	}
	if rows[0]["expired"] != false {
		t.Fatalf("a request seen just now reads as expired: %+v", rows[0])
	}
	if rows[0]["toolName"] != "Bash" || rows[0]["id"] != "perm-1" {
		t.Fatalf("request lost its detail: %+v", rows[0])
	}
	if _, leaked := rows[0]["seenAt"]; leaked {
		t.Fatal("seenAt reached the client; it is bookkeeping, not data")
	}

	// Past the grace period the daemon has handed the prompt to the agent's dialog.
	live.mu.Lock()
	live.permissions["perm-1"]["seenAt"] = time.Now().Add(-time.Second)
	live.mu.Unlock()
	if live.permissionRows()[0]["expired"] != true {
		t.Fatal("a request older than the grace period still reads as answerable")
	}

	live.apply(map[string]any{"type": "permission:resolved", "requestId": "perm-1"})
	if got := len(live.permissionRows()); got != 0 {
		t.Fatalf("%d requests after resolve, want 0", got)
	}
}

// The drift counter is the only way anyone would notice the apply rules going wrong, so
// it has to actually count.
func TestResyncCountsWhatItHadToCorrect(t *testing.T) {
	newFakeDaemon(t, listAnswers(
		[]any{
			map[string]any{"id": "s1", "status": "finished"},
			map[string]any{"id": "s3", "status": "running"},
		},
		nil, nil,
	))
	coldStore()
	live.mu.Lock()
	live.sessions["s1"] = map[string]any{"id": "s1", "status": "running"} // stale
	live.sessions["s2"] = map[string]any{"id": "s2", "status": "running"} // gone
	live.syncedAt = time.Now()                                            // so resync compares
	live.mu.Unlock()

	live.resync()

	live.mu.RLock()
	drift := live.drift
	live.mu.RUnlock()
	// s1 changed, s2 vanished, s3 appeared.
	if drift != 3 {
		t.Fatalf("drift = %d, want 3", drift)
	}
	if got := live.session("s1")["status"]; got != "finished" {
		t.Fatalf("resync did not correct the stale row: %v", got)
	}
	if live.session("s2") != nil {
		t.Fatal("resync kept a session the daemon no longer lists")
	}
	if health := live.health(); health["drift"] != 3 {
		t.Fatalf("diagnostics reports drift %v, want 3", health["drift"])
	}
}

// An agreeing resync must not report drift, or the number means nothing.
func TestResyncReportsNoDriftWhenItAgrees(t *testing.T) {
	rows := []any{map[string]any{"id": "s1", "status": "running"}}
	newFakeDaemon(t, listAnswers(rows, nil, nil))
	coldStore()

	live.resync() // first pass sets the baseline
	live.resync() // second pass compares against it
	live.mu.RLock()
	defer live.mu.RUnlock()
	if live.drift != 0 {
		t.Fatalf("drift = %d on an agreeing resync, want 0", live.drift)
	}
}

func TestSubscribersHearChanges(t *testing.T) {
	coldStore()
	warmStore()
	ch, stop := live.subscribe()
	defer stop()

	if c, ok := live.apply(map[string]any{"type": "session:updated", "session": map[string]any{"id": "s1"}}); ok {
		live.publish(c)
	}
	select {
	case c := <-ch:
		if c.Kind != "session" || c.ID != "s1" {
			t.Fatalf("heard %+v", c)
		}
	case <-time.After(time.Second):
		t.Fatal("heard nothing")
	}

	stop()
	if _, open := <-ch; open {
		t.Fatal("the channel stayed open after stop")
	}
}

// A phone that stops reading must not hold the watcher up. It gets dropped, and its
// reconnect re-reads everything, which is the state it would have caught up to anyway.
func TestASlowSubscriberIsDropped(t *testing.T) {
	coldStore()
	warmStore()
	ch, stop := live.subscribe()
	defer stop()

	for i := 0; i < 200; i++ {
		live.publish(change{Kind: "sessions"})
	}
	drained := 0
	for range ch {
		drained++
	}
	if drained == 0 {
		t.Fatal("the subscriber got nothing at all")
	}
	live.mu.RLock()
	defer live.mu.RUnlock()
	if len(live.subs) != 0 {
		t.Fatalf("%d subscribers left, want the slow one dropped", len(live.subs))
	}
}
