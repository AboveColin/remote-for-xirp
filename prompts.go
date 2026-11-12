package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Saved prompts are the ones you keep on the desktop, added to Xirp in 0.19.1. Reading
// them from a phone turns a paragraph typed with thumbs into one tap.
//
// The daemon validates the entire list on every write and refuses it whole, so the
// shape is not negotiable. From modules/saved-prompts/validation.js in Xirp 0.22.0:
//
//   - the only accepted keys are id, name, prompt, projectId, createdAt and updatedAt.
//     One extra key fails the write, and the daemon reports that failure as the single
//     word "validation".
//   - id is a UUID of version 1 to 8 with variant 8, 9, a or b, and unique in the list.
//   - name is 1 to 120 characters, prompt is 1 to 50000.
//   - createdAt and updatedAt must equal exactly what JavaScript's toISOString()
//     prints: three decimal places and a literal Z. Go's time.RFC3339Nano is refused,
//     because it prints nanoseconds and trims trailing zeros.
//   - updatedAt is not before createdAt. At most 100 prompts, at most 1 MiB of JSON.
//
// Two things a reader should know about writing them.
//
// `chirp:savedPrompts:set` replaces the whole list, so this reads, changes and writes
// back inside one request. A prompt added on the desktop within that window, about one
// round trip, is lost. That is the narrowest this can be without a merge the daemon
// does not offer.
//
// The daemon also refuses every write while the stored list holds an entry it considers
// invalid, and reports that as "validation" too. Such a list reads back as empty. So "no
// prompts" here and "refused" on a write can mean the same thing.
const (
	maxPrompts     = 100
	maxPromptName  = 120
	maxPromptText  = 50000
	promptTimeForm = "2006-01-02T15:04:05.000Z"
)

// promptFields is the whole allowed key set. Anything else fails the daemon's validator.
var promptFields = []string{"id", "name", "prompt", "projectId", "createdAt", "updatedAt"}

// newPromptID makes a version-4 UUID, which is the only id shape the validator accepts.
func newPromptID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// promptRefusal turns the daemon's one-word refusal into something a person can act on.
func promptRefusal(word string) string {
	switch word {
	case "validation":
		return "Xirp refused the write. It refuses every change while the saved list holds an entry it considers invalid, so open Saved prompts on the desktop and re-save them."
	case "persistence":
		return "Xirp could not write the prompts to its settings store."
	}
	return "Xirp refused the write: " + word
}

func readPrompts() ([]any, error) {
	res, err := client.Call(map[string]any{"type": "chirp:savedPrompts:get"}, "chirp:savedPrompts", 15*time.Second)
	if err != nil {
		return nil, err
	}
	if why, _ := res["error"].(string); why != "" {
		return nil, errors.New(promptRefusal(why))
	}
	list, _ := res["prompts"].([]any)
	return list, nil
}

func promptID(entry any) string {
	if m, ok := entry.(map[string]any); ok {
		id, _ := m["id"].(string)
		return id
	}
	return ""
}

// handlePrompts serves the saved prompts, and writes one back.
func handlePrompts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		writePrompt(w, r)
		return
	}
	list, err := readPrompts()
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		if pm, ok := p.(map[string]any); ok {
			out = append(out, project(pm, promptFields))
		}
	}
	writeJSON(w, 200, map[string]any{"prompts": out})
}

func writePrompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
		Delete string `json:"delete"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad body"})
		return
	}

	current, err := readPrompts()
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}

	next, saved, err := applyPrompt(current, body.Delete, body.ID, body.Name, body.Prompt, time.Now())
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}

	rid, err := newPromptID()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	res, err := client.Call(map[string]any{
		"type": "chirp:savedPrompts:set", "prompts": next, "requestId": rid,
	}, "chirp:savedPrompts", 15*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	if why, _ := res["error"].(string); why != "" {
		writeJSON(w, 409, map[string]any{"error": promptRefusal(why)})
		return
	}

	if body.Delete != "" {
		log.Printf("deleted saved prompt %s (%d left)", body.Delete, len(next))
	} else {
		log.Printf("saved prompt %q (%d total)", saved["name"], len(next))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "prompt": saved, "count": len(next)})
}

// applyPrompt builds the list to write back. It is separate from the request so the
// shape rules can be tested without a daemon, and it takes `at` so the timestamps in a
// test are fixed.
func applyPrompt(current []any, remove, id, name, text string, at time.Time) (next []any, saved map[string]any, err error) {
	if remove != "" {
		for _, p := range current {
			if promptID(p) != remove {
				next = append(next, p)
			}
		}
		if len(next) == len(current) {
			return nil, nil, fmt.Errorf("no saved prompt with id %s", remove)
		}
		if next == nil {
			next = []any{}
		}
		return next, map[string]any{"id": remove, "deleted": true}, nil
	}

	name = strings.TrimSpace(name)
	text = strings.TrimSpace(text)
	switch {
	case name == "":
		return nil, nil, errors.New("a name is required")
	case text == "":
		return nil, nil, errors.New("the prompt text is required")
	case len(name) > maxPromptName:
		return nil, nil, fmt.Errorf("the name is %d characters, over the %d Xirp accepts", len(name), maxPromptName)
	case len(text) > maxPromptText:
		return nil, nil, fmt.Errorf("the prompt is %d characters, over the %d Xirp accepts", len(text), maxPromptText)
	}

	stamp := at.UTC().Format(promptTimeForm)
	entry := map[string]any{"name": name, "prompt": text, "updatedAt": stamp}

	replaced := false
	for _, p := range current {
		if id != "" && promptID(p) == id {
			was, _ := p.(map[string]any)
			entry["id"] = id
			// createdAt has to survive an edit: the daemon refuses a list whose
			// updatedAt is before its createdAt.
			if made, _ := was["createdAt"].(string); made != "" {
				entry["createdAt"] = made
			} else {
				entry["createdAt"] = stamp
			}
			if pid, _ := was["projectId"].(string); pid != "" {
				entry["projectId"] = pid
			}
			next = append(next, entry)
			replaced = true
			continue
		}
		next = append(next, p)
	}
	if !replaced {
		if id != "" {
			return nil, nil, fmt.Errorf("no saved prompt with id %s", id)
		}
		fresh, idErr := newPromptID()
		if idErr != nil {
			return nil, nil, idErr
		}
		entry["id"] = fresh
		entry["createdAt"] = stamp
		next = append(next, entry)
	}
	if len(next) > maxPrompts {
		return nil, nil, fmt.Errorf("that would be %d saved prompts, over the %d Xirp keeps", len(next), maxPrompts)
	}
	return next, entry, nil
}
