package main

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The daemon's own id and timestamp rules, from modules/saved-prompts/validation.js in
// Xirp 0.22.0. A payload that misses either is refused with the single word
// "validation", which says nothing about which field was wrong.
var (
	promptUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	promptTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
)

func TestNewPromptIDMatchesTheDaemonsPattern(t *testing.T) {
	for i := 0; i < 200; i++ {
		id, err := newPromptID()
		if err != nil {
			t.Fatalf("newPromptID: %v", err)
		}
		if !promptUUID.MatchString(id) {
			t.Fatalf("id %q does not match the daemon's pattern", id)
		}
	}
}

// Go's RFC3339Nano prints nanoseconds and trims trailing zeros, and the daemon compares
// the string against JavaScript's toISOString() byte for byte. This is the test that
// keeps someone from swapping the format constant for the familiar one.
func TestPromptTimestampsLookLikeToISOString(t *testing.T) {
	at := time.Date(2026, 9, 1, 14, 30, 0, 0, time.UTC) // whole second: .000
	next, saved, err := applyPrompt(nil, "", "", "Ship it", "Review the diff and ship it.", at)
	if err != nil {
		t.Fatalf("applyPrompt: %v", err)
	}
	if len(next) != 1 {
		t.Fatalf("list has %d entries, want 1", len(next))
	}
	for _, field := range []string{"createdAt", "updatedAt"} {
		got, _ := saved[field].(string)
		if !promptTime.MatchString(got) {
			t.Fatalf("%s = %q, which toISOString() would not print", field, got)
		}
	}
	if got := saved["createdAt"]; got != "2026-09-01T14:30:00.000Z" {
		t.Fatalf("createdAt = %v", got)
	}
	if bad := time.RFC3339Nano; promptTimeForm == bad {
		t.Fatal("the format constant is RFC3339Nano, which the daemon refuses")
	}
}

// One key the validator does not know fails the whole write, so the payload may carry
// nothing beyond its six allowed fields.
func TestSavedPromptCarriesNoExtraKeys(t *testing.T) {
	allowed := map[string]bool{}
	for _, f := range promptFields {
		allowed[f] = true
	}
	next, _, err := applyPrompt(nil, "", "", "Ship it", "Review the diff.", time.Now())
	if err != nil {
		t.Fatalf("applyPrompt: %v", err)
	}
	entry := next[0].(map[string]any)
	for k := range entry {
		if !allowed[k] {
			t.Fatalf("entry carries %q, which is not one of %v", k, promptFields)
		}
	}
}

// An edit keeps the original createdAt: the daemon refuses a list whose updatedAt is
// before its createdAt.
func TestEditingAPromptKeepsItsCreatedAt(t *testing.T) {
	current := []any{map[string]any{
		"id":        "6f1c0a5e-1c2b-4d3e-8a9b-0c1d2e3f4a5b",
		"name":      "old name",
		"prompt":    "old text",
		"createdAt": "2026-08-01T09:00:00.000Z",
		"updatedAt": "2026-08-01T09:00:00.000Z",
	}}
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	_, saved, err := applyPrompt(current, "", "6f1c0a5e-1c2b-4d3e-8a9b-0c1d2e3f4a5b", "new name", "new text", at)
	if err != nil {
		t.Fatalf("applyPrompt: %v", err)
	}
	if got := saved["createdAt"]; got != "2026-08-01T09:00:00.000Z" {
		t.Fatalf("createdAt = %v, want the original", got)
	}
	if got := saved["updatedAt"]; got != "2026-09-01T10:00:00.000Z" {
		t.Fatalf("updatedAt = %v", got)
	}
}

func TestPromptLimitsNameTheLimitAndTheAsk(t *testing.T) {
	long := strings.Repeat("x", maxPromptName+1)
	if _, _, err := applyPrompt(nil, "", "", long, "text", time.Now()); err == nil {
		t.Fatal("a 121-character name was accepted")
	} else if !strings.Contains(err.Error(), "121") || !strings.Contains(err.Error(), "120") {
		t.Fatalf("error %q names neither the ask nor the limit", err)
	}

	full := make([]any, maxPrompts)
	for i := range full {
		full[i] = map[string]any{"id": "x"}
	}
	if _, _, err := applyPrompt(full, "", "", "one more", "text", time.Now()); err == nil {
		t.Fatal("a 101st prompt was accepted")
	} else if !strings.Contains(err.Error(), "101") || !strings.Contains(err.Error(), "100") {
		t.Fatalf("error %q names neither the ask nor the limit", err)
	}
}

func TestDeletingAnUnknownPromptSaysSo(t *testing.T) {
	if _, _, err := applyPrompt(nil, "nope", "", "", "", time.Now()); err == nil {
		t.Fatal("deleting an id that is not there succeeded")
	}
}

// A refusal comes back as one word inside the reply of the success type. Left unread it
// reads as a saved prompt that was never saved.
func TestSavePromptSurfacesARefusal(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		switch req["type"] {
		case "chirp:savedPrompts:get":
			send(map[string]any{"type": "chirp:savedPrompts", "prompts": []any{}})
		case "chirp:savedPrompts:set":
			send(map[string]any{
				"type": "chirp:savedPrompts", "prompts": []any{},
				"requestId": req["requestId"], "error": "validation",
			})
		}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/prompts", strings.NewReader(`{"name":"Ship it","prompt":"Review the diff."}`))
	handlePrompts(rec, req)
	if rec.Code != 409 {
		t.Fatalf("status %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "re-save") {
		t.Fatalf("answer %q does not say what to do about it", rec.Body.String())
	}
}

func TestPromptsRoundTripThroughTheDaemon(t *testing.T) {
	var wrote []any
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		switch req["type"] {
		case "chirp:savedPrompts:get":
			send(map[string]any{"type": "chirp:savedPrompts", "prompts": wrote})
		case "chirp:savedPrompts:set":
			wrote, _ = req["prompts"].([]any)
			send(map[string]any{"type": "chirp:savedPrompts", "prompts": wrote, "requestId": req["requestId"]})
		}
	})

	post := func(payload string) map[string]any {
		rec := httptest.NewRecorder()
		handlePrompts(rec, httptest.NewRequest("POST", "/api/prompts", strings.NewReader(payload)))
		if rec.Code != 200 {
			t.Fatalf("status %d (%s)", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("answer is not JSON: %v", err)
		}
		return out
	}

	out := post(`{"name":"Ship it","prompt":"Review the diff and ship it."}`)
	saved, _ := out["prompt"].(map[string]any)
	id, _ := saved["id"].(string)
	if !promptUUID.MatchString(id) {
		t.Fatalf("saved id %q", id)
	}

	rec := httptest.NewRecorder()
	handlePrompts(rec, httptest.NewRequest("GET", "/api/prompts", nil))
	var listed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list is not JSON: %v", err)
	}
	if got := len(listed["prompts"].([]any)); got != 1 {
		t.Fatalf("listed %d prompts, want 1", got)
	}

	out = post(`{"delete":"` + id + `"}`)
	if got, _ := out["count"].(float64); got != 0 {
		t.Fatalf("count after delete = %v, want 0", got)
	}
}
