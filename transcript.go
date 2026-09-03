package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Reading a transcript means spawning squab, which returns every message in the session:
// measured at 5,244,612 bytes for a 1035-message session, in 0.12 s. The phone only ever
// reads the newest 200, and the detail screen re-reads whenever the session changes, so
// that whole file used to move through this process every time.
//
// squab takes --since, which returns only what came after a timestamp: 57,781 bytes for
// that same session, in 0.10 s. So a session is read in full once and topped up after
// that. The saving is bytes, not time, because node's startup and reading the JSONL
// dominate either way.
//
// Three things about the data decide how the top-up merges, each measured rather than
// assumed:
//
//   - --since is strictly after the timestamp given, so appending what comes back is
//     correct and nothing needs deduplicating.
//   - message ids are not unique. That session had 1035 messages under 681 ids, because
//     a tool call and its result share one. Merging by id would drop every tool result.
//   - messageCount counts the whole session in every answer, including a --since answer.
//     So kept plus dropped has to equal it, and when it does not, something rewrote the
//     file and the honest answer is to read it again.

// maxKept is how many messages a session holds in memory. The detail endpoint caps a
// request at 200, so anything past that could not be asked for. Measured at about 2 kB
// per message, this is roughly 480 kB for a busy session.
const maxKept = 240

// maxTranscripts bounds the cache. A phone has a handful of sessions open at once, and at
// maxKept each that is a few megabytes. The least recently read one goes first.
const maxTranscripts = 8

const defaultSquabPath = "/Applications/Xirp.app/Contents/Resources/app.asar.unpacked/node_modules/@chirp/squab/dist/cli.js"

// sessionTranscript is one session's tail, plus what squab says about the whole of it.
type sessionTranscript struct {
	messages   []any
	aggregates map[string]any
	dropped    int    // messages trimmed off the front, so the count guard still adds up
	newest     string // the timestamp to ask --since with next time
	usedAt     time.Time
}

var (
	transcriptMu      sync.Mutex
	transcripts       = map[string]*sessionTranscript{}
	transcriptReads   int // squab runs
	transcriptRepairs int // times the count guard forced a full re-read
)

// ParseSession returns a session's transcript, reading only what is new since it was last
// asked. The daemon's own messages:list cannot answer this: its database is empty for
// harness-driven sessions, which is why squab exists.
func (c *Client) ParseSession(sessionID string, limit int) (map[string]any, error) {
	transcriptMu.Lock()
	defer transcriptMu.Unlock()

	held := transcripts[sessionID]
	if held == nil {
		fresh, err := readTranscript(sessionID, "")
		if err != nil {
			return nil, err
		}
		held = fresh
		transcripts[sessionID] = held
		evictTranscripts()
	} else if err := topUp(sessionID, held); err != nil {
		return nil, err
	}

	held.usedAt = time.Now()
	return held.answer(limit), nil
}

// topUp asks for what came after the newest message held, and reads the whole session
// again when the count says the file changed underneath.
func topUp(sessionID string, held *sessionTranscript) error {
	add, err := readTranscript(sessionID, held.newest)
	if err != nil {
		return err
	}
	held.aggregates = add.aggregates
	held.messages = append(held.messages, add.messages...)
	if len(add.messages) > 0 {
		held.newest = add.newest
	}
	held.trim()

	total, ok := held.aggregates["messageCount"].(float64)
	if !ok || held.dropped+len(held.messages) == int(total) {
		return nil
	}
	// The arithmetic disagrees, so the file is not the one this was built from: a
	// compaction, a rewrite, or two messages sharing the timestamp the top-up asked
	// from. Read it whole.
	transcriptRepairs++
	fresh, err := readTranscript(sessionID, "")
	if err != nil {
		return err
	}
	*held = *fresh
	return nil
}

// answer shapes the reply the detail endpoint sends, newest `limit` messages last.
func (t *sessionTranscript) answer(limit int) map[string]any {
	out := make(map[string]any, len(t.aggregates)+3)
	for k, v := range t.aggregates {
		out[k] = v
	}
	msgs := t.messages
	total := t.dropped + len(msgs)
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	out["messages"] = msgs
	out["totalMessages"] = total
	out["truncatedFromStart"] = len(msgs) < total
	return out
}

// trim keeps the tail and remembers how much it let go of.
func (t *sessionTranscript) trim() {
	if extra := len(t.messages) - maxKept; extra > 0 {
		t.messages = append([]any{}, t.messages[extra:]...)
		t.dropped += extra
	}
}

// evictTranscripts drops the least recently read session once the cache is over its cap.
func evictTranscripts() {
	for len(transcripts) > maxTranscripts {
		oldest, at := "", time.Now()
		for id, t := range transcripts {
			if t.usedAt.Before(at) || oldest == "" {
				oldest, at = id, t.usedAt
			}
		}
		delete(transcripts, oldest)
	}
}

// readTranscript runs squab once. `since` empty reads the whole session.
//
// No --limit: its limit is a HEAD, not a tail. `--limit 3` on a 1111-message session
// returns messages 0, 1 and 2, so passing the phone's page size pinned every conversation
// to its opening exchange and it never appeared to update.
func readTranscript(sessionID, since string) (*sessionTranscript, error) {
	squab := os.Getenv("CHIRP_SQUAB_PATH")
	if squab == "" {
		squab = defaultSquabPath
	}
	if _, err := os.Stat(squab); err != nil {
		return nil, fmt.Errorf("squab CLI not found at %s", squab)
	}
	args := []string{squab, "session-parse", sessionID}
	if since != "" {
		args = append(args, "--since", since)
	}

	transcriptReads++
	out, err := exec.Command("node", args...).Output() //nolint:gosec // fixed argv, no shell
	if err != nil {
		return nil, fmt.Errorf("squab session-parse %s: %w", sessionID, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("squab returned non-JSON for %s: %w", sessionID, err)
	}

	msgs, _ := parsed["messages"].([]any)
	t := &sessionTranscript{
		messages:   msgs,
		aggregates: map[string]any{},
		usedAt:     time.Now(),
	}
	for k, v := range parsed {
		if k != "messages" {
			t.aggregates[k] = v
		}
	}
	if n := len(msgs); n > 0 {
		if last, ok := msgs[n-1].(map[string]any); ok {
			t.newest, _ = last["ts"].(string)
		}
	}
	t.trim()
	return t, nil
}

// transcriptHealth is what diagnostics reports: how many sessions are held, how many
// squab runs it took, and how often the count guard had to read a session again.
func transcriptHealth() map[string]any {
	transcriptMu.Lock()
	defer transcriptMu.Unlock()
	held := 0
	for _, t := range transcripts {
		held += len(t.messages)
	}
	return map[string]any{
		"sessions": len(transcripts),
		"messages": held,
		"reads":    transcriptReads,
		"repairs":  transcriptRepairs,
	}
}
