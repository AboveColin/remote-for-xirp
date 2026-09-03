package main

import "testing"

func bodyFor(t *testing.T, events []pushEvent, id string) string {
	t.Helper()
	for _, e := range events {
		if e.id == id {
			return e.body
		}
	}
	return ""
}

// `finished` is the status a turn lands in when nobody is at the desk, which is the
// whole case this app exists for. Receipt, Xirp 0.22.0 notifications/service.js: on the
// agent's stop hook the daemon sets `idle` when a viewer is watching, leaves the
// session `running` when background agents are still going, and otherwise sets
// `finished`. `completed` is a different event: the agent exited or the session was
// stopped.
func TestFinishedTurnIsWorthANotification(t *testing.T) {
	names := map[string]string{"p1": "webapp"}
	prev := map[string]string{"s1": "running"}
	sessions := []map[string]any{
		{"id": "s1", "name": "Fix the flake", "status": "finished", "projectId": "p1"},
	}

	events, now := statusEvents(sessions, names, prev)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: a finished turn sends nothing", len(events))
	}
	if events[0].title != "Fix the flake" {
		t.Fatalf("title = %q", events[0].title)
	}
	if got := events[0].body; got != "Finished in webapp" {
		t.Fatalf("body = %q, want %q", got, "Finished in webapp")
	}
	if now["s1"] != "finished" {
		t.Fatalf("remembered status = %q, want finished", now["s1"])
	}
}

func TestStatusEventsRules(t *testing.T) {
	names := map[string]string{"p1": "webapp"}
	prev := map[string]string{
		"done": "running", "gone": "running", "broke": "running",
		"asks": "running", "child": "running", "watched": "running", "same": "running",
	}
	sessions := []map[string]any{
		{"id": "done", "name": "done", "status": "finished", "projectId": "p1"},
		{"id": "gone", "name": "gone", "status": "completed", "projectId": "p1"},
		{"id": "broke", "name": "broke", "status": "failed", "projectId": "p1"},
		{"id": "asks", "name": "asks", "status": "waiting", "projectId": "p1", "waitingReason": "needs your answer"},
		// A child waiting on its parent is not something a person can act on.
		{"id": "child", "name": "child", "status": "waiting_on_parent", "projectId": "p1"},
		// `idle` is the same turn end as `finished`, but with someone at the desk.
		{"id": "watched", "name": "watched", "status": "idle", "projectId": "p1"},
		{"id": "same", "name": "same", "status": "running", "projectId": "p1"},
	}

	events, _ := statusEvents(sessions, names, prev)
	want := map[string]string{
		"done":  "Finished in webapp",
		"gone":  "Ended in webapp",
		"broke": "Failed in webapp",
		"asks":  "needs your answer",
	}
	if len(events) != len(want) {
		var got []string
		for _, e := range events {
			got = append(got, e.id+"="+e.body)
		}
		t.Fatalf("got %d events %v, want %d", len(events), got, len(want))
	}
	for id, body := range want {
		if got := bodyFor(t, events, id); got != body {
			t.Errorf("%s: body = %q, want %q", id, got, body)
		}
	}
}

// The first pass has nothing to compare against, so it must stay quiet rather than
// announce every session that finished hours ago.
func TestFirstPassIsQuiet(t *testing.T) {
	sessions := []map[string]any{
		{"id": "s1", "name": "old", "status": "completed", "projectId": "p1"},
		{"id": "s2", "name": "older", "status": "failed", "projectId": "p1"},
	}
	events, now := statusEvents(sessions, map[string]string{}, map[string]string{})
	if len(events) != 0 {
		t.Fatalf("got %d events on the first pass, want 0", len(events))
	}
	if len(now) != 2 {
		t.Fatalf("remembered %d statuses, want 2", len(now))
	}
}

// A session that no longer exists must not stay in the table for the life of the
// process, and an unknown project must not read as "Finished in ".
func TestStatusEventsForgetsAndPlaces(t *testing.T) {
	prev := map[string]string{"s1": "running", "vanished": "running"}
	sessions := []map[string]any{{"id": "s1", "status": "finished"}}
	events, now := statusEvents(sessions, map[string]string{}, prev)
	if _, still := now["vanished"]; still {
		t.Fatal("a session the daemon no longer lists is still remembered")
	}
	if len(events) != 1 || events[0].body != "Finished" {
		t.Fatalf("events = %+v, want one body %q", events, "Finished")
	}
	// No name from the daemon: the id is the label, and it must not be sliced past
	// its end.
	if events[0].title != "s1" {
		t.Fatalf("title = %q, want the short id", events[0].title)
	}
}
