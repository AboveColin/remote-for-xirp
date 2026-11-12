package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSquab writes a script that answers session-parse the way the real one does, and
// records the arguments it was called with. The real squab lives inside the Xirp app, so
// this is how the merge rules get tested anywhere.
func fakeSquab(t *testing.T, messages int) (calls func() []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "squab.js")

	// One message per second from a fixed start, so timestamps are predictable and
	// --since can be reasoned about. The count is read from a file, so a test can grow
	// the session between reads.
	body := fmt.Sprintf(`
const fs = require('fs');
fs.appendFileSync(%q, process.argv.slice(2).join(' ') + '\n');
const total = Number(fs.readFileSync(%q, 'utf8').trim());
const since = process.argv.includes('--since') ? process.argv[process.argv.indexOf('--since') + 1] : null;
const start = Date.parse('2026-09-01T00:00:00.000Z');
const all = [];
for (let i = 0; i < total; i++) {
  all.push({ id: 'm' + i, ts: new Date(start + i * 1000).toISOString(), role: 'assistant', type: 'message', text: 'line ' + i });
}
const kept = since ? all.filter((m) => Date.parse(m.ts) > Date.parse(since)) : all;
process.stdout.write(JSON.stringify({ schema: 'squab.session-parsed/v1', messageCount: all.length, messages: kept }));
`, log, filepath.Join(dir, "total"))
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "total"), []byte(fmt.Sprint(messages)), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CHIRP_SQUAB_PATH", script)
	transcriptMu.Lock()
	transcripts = map[string]*sessionTranscript{}
	transcriptReads, transcriptRepairs = 0, 0
	transcriptMu.Unlock()

	return func() []string {
		b, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}, dir
}

func grow(t *testing.T, dir string, messages int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "total"), []byte(fmt.Sprint(messages)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The first read takes the whole session. Every read after it asks only for what came
// after the newest message held, which is where the 90x fewer bytes come from.
func TestTranscriptReadsWholeSessionOnceThenTopsUp(t *testing.T) {
	calls, dir := fakeSquab(t, 10)

	first, err := client.ParseSession("s1", 5)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if got := first["totalMessages"]; got != 10 {
		t.Fatalf("totalMessages = %v, want 10", got)
	}
	if msgs, _ := first["messages"].([]any); len(msgs) != 5 {
		t.Fatalf("returned %d messages, want the 5 asked for", len(msgs))
	}
	if got := calls(); len(got) != 1 || strings.Contains(got[0], "--since") {
		t.Fatalf("first call was %v, want one whole read", got)
	}

	grow(t, dir, 13)
	second, err := client.ParseSession("s1", 5)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got := second["totalMessages"]; got != 13 {
		t.Fatalf("totalMessages = %v, want 13", got)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("%d squab runs, want 2", len(got))
	}
	// The ninth message is the newest the first read held, so the top-up asks from it.
	if want := "--since 2026-09-01T00:00:09.000Z"; !strings.Contains(got[1], want) {
		t.Fatalf("top-up ran %q, want it to contain %q", got[1], want)
	}

	// And the merge has to be the same transcript a whole read would have produced.
	msgs, _ := second["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["id"] != "m12" {
		t.Fatalf("newest message is %v, want m12", last["id"])
	}
	if first, _ := msgs[0].(map[string]any); first["id"] != "m8" {
		t.Fatalf("oldest of the 5 returned is %v, want m8", first["id"])
	}
}

// Nothing new means nothing added, and no duplicate at the boundary.
func TestTopUpWithNothingNewChangesNothing(t *testing.T) {
	_, _ = fakeSquab(t, 6)
	if _, err := client.ParseSession("s1", 10); err != nil {
		t.Fatal(err)
	}
	again, err := client.ParseSession("s1", 10)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := again["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("%d messages after a top-up with nothing new, want 6", len(msgs))
	}
	seen := map[string]int{}
	for _, m := range msgs {
		row, _ := m.(map[string]any)
		id, _ := row["id"].(string)
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("message %s appears %d times", id, n)
		}
	}
}

// A transcript that was rewritten under the cache cannot be topped up, and the message
// count is what says so. The answer is to read it again rather than serve a mixture.
func TestARewrittenTranscriptIsReadAgain(t *testing.T) {
	_, dir := fakeSquab(t, 20)
	if _, err := client.ParseSession("s1", 5); err != nil {
		t.Fatal(err)
	}
	// The session shrank, which only a rewrite can do. The top-up returns nothing, so
	// the arithmetic disagrees: 20 held against a count of 8.
	grow(t, dir, 8)

	out, err := client.ParseSession("s1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := out["totalMessages"]; got != 8 {
		t.Fatalf("totalMessages = %v, want 8 after the rewrite", got)
	}
	transcriptMu.Lock()
	repairs := transcriptRepairs
	transcriptMu.Unlock()
	if repairs != 1 {
		t.Fatalf("repairs = %d, want 1", repairs)
	}
	if got := transcriptHealth()["repairs"]; got != 1 {
		t.Fatalf("diagnostics reports repairs = %v, want 1", got)
	}
}

// Memory is bounded by what the endpoint can ask for, and the count guard has to keep
// working after the trim, which is what `dropped` is for.
func TestATranscriptKeepsOnlyTheTail(t *testing.T) {
	_, _ = fakeSquab(t, maxKept+50)
	out, err := client.ParseSession("s1", 200)
	if err != nil {
		t.Fatal(err)
	}
	if got := out["totalMessages"]; got != maxKept+50 {
		t.Fatalf("totalMessages = %v, want %d", got, maxKept+50)
	}
	if out["truncatedFromStart"] != true {
		t.Fatal("a session longer than the cap must say the start is missing")
	}
	transcriptMu.Lock()
	held := transcripts["s1"]
	kept, dropped := len(held.messages), held.dropped
	transcriptMu.Unlock()
	if kept != maxKept || dropped != 50 {
		t.Fatalf("kept %d and dropped %d, want %d and 50", kept, dropped, maxKept)
	}

	// The guard still has to add up, so a top-up must not force a repair.
	if _, err := client.ParseSession("s1", 200); err != nil {
		t.Fatal(err)
	}
	transcriptMu.Lock()
	repairs := transcriptRepairs
	transcriptMu.Unlock()
	if repairs != 0 {
		t.Fatalf("repairs = %d after trimming, want 0: the count guard is off by the trim", repairs)
	}
}

func TestTranscriptCacheEvictsTheLeastRecentlyRead(t *testing.T) {
	_, _ = fakeSquab(t, 3)
	for i := 0; i < maxTranscripts+2; i++ {
		if _, err := client.ParseSession(fmt.Sprintf("s%d", i), 5); err != nil {
			t.Fatal(err)
		}
		// Distinct read times, so "least recently" means something.
		time.Sleep(time.Millisecond)
	}
	transcriptMu.Lock()
	defer transcriptMu.Unlock()
	if len(transcripts) != maxTranscripts {
		t.Fatalf("%d sessions cached, want the cap of %d", len(transcripts), maxTranscripts)
	}
	if _, still := transcripts["s0"]; still {
		t.Fatal("the first session read is still cached")
	}
	if _, ok := transcripts[fmt.Sprintf("s%d", maxTranscripts+1)]; !ok {
		t.Fatal("the most recent session was evicted")
	}
}

// A missing squab is the normal state on a machine without Xirp, and it has to say so
// rather than look like an empty transcript.
func TestMissingSquabSaysWhereItLooked(t *testing.T) {
	t.Setenv("CHIRP_SQUAB_PATH", filepath.Join(t.TempDir(), "nope.js"))
	transcriptMu.Lock()
	transcripts = map[string]*sessionTranscript{}
	transcriptMu.Unlock()

	_, err := client.ParseSession("s1", 5)
	if err == nil {
		t.Fatal("want an error when squab is not there")
	}
	if !strings.Contains(err.Error(), "nope.js") {
		t.Fatalf("error %q does not say where it looked", err)
	}
}

// The endpoint's contract has to survive the rewrite: the same keys, and the newest
// messages last.
func TestTranscriptAnswerKeepsTheEndpointContract(t *testing.T) {
	_, _ = fakeSquab(t, 4)
	out, err := client.ParseSession("s1", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"messages", "messageCount", "totalMessages", "truncatedFromStart", "schema"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("the answer has no %q: %v", key, keysOf(out))
		}
	}
	b, _ := json.Marshal(out["messages"])
	if !strings.Contains(string(b), "m3") || strings.Contains(string(b), "m1") {
		t.Fatalf("messages are not the newest two: %s", b)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
