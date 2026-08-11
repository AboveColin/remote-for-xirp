package main

import (
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The terminal view shows the session's actual tmux pane, escape codes and all.
//
// This is deliberately not a reconstruction of the conversation. The pane is what
// the agent's TUI is drawing right now, so slash-command menus, model pickers,
// permission prompts and progress spinners all appear without this wrapper needing
// to know what any of them are. Keystrokes go back through session:message, so the
// pane and the input are the same channel the desktop uses.
//
// `capture-pane -e` keeps the SGR sequences; the browser renders them. `-J` joins
// wrapped lines so a narrow phone does not re-wrap already-wrapped output.
func sessionPane(w http.ResponseWriter, r *http.Request, id string) {
	res, err := client.Call(map[string]any{"type": "session:get", "sessionId": id}, "session:get", 15*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	sm, _ := res["session"].(map[string]any)
	if sm == nil {
		writeJSON(w, 404, map[string]any{"error": "session not found"})
		return
	}
	target, _ := sm["tmuxSession"].(string)
	if target == "" {
		// SDK-mode sessions have no pane at all; say so rather than showing an
		// empty terminal that looks broken.
		writeJSON(w, 200, map[string]any{"unavailable": "this session has no tmux pane"})
		return
	}

	lines := 200
	if q := r.URL.Query().Get("lines"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 2000 {
			lines = n
		}
	}

	// -S -N starts N lines back in the scrollback, so the view has history to scroll
	// through rather than only the visible screen.
	out, err := exec.Command("tmux", "capture-pane", "-p", "-e", "-J",
		"-S", "-"+strconv.Itoa(lines), "-t", target).Output() //nolint:gosec // fixed argv
	if err != nil {
		writeJSON(w, 200, map[string]any{"unavailable": fmt.Sprintf("pane %s not capturable: %v", target, err)})
		return
	}

	size := ""
	if g, err := exec.Command("tmux", "display", "-p", "-t", target, "#{pane_width}x#{pane_height}").Output(); err == nil { //nolint:gosec // fixed argv
		size = strings.TrimSpace(string(g))
	}

	text := string(out)
	// Trailing blank lines are most of a fresh pane and just push the live content
	// off a phone screen.
	text = strings.TrimRight(text, "\n \t")

	writeJSON(w, 200, map[string]any{
		"pane": target,
		"size": size,
		"text": text,
		"at":   time.Now().Format(time.RFC3339),
	})
}

// sessionKeys sends raw keys to the pane: Escape, Tab, arrows, Ctrl-C. The chat
// composer can only send text, but a TUI needs the rest — you cannot pick from a
// slash-command menu without arrows and Enter.
//
// Only an allowlist of key names is accepted. Passing arbitrary strings through to
// `tmux send-keys` would let anything type anything into any pane.
var allowedKeys = map[string]string{
	"escape": "Escape",
	"tab":    "Tab",
	"up":     "Up",
	"down":   "Down",
	"left":   "Left",
	"right":  "Right",
	"enter":  "Enter",
	"ctrl-c": "C-c",
	"ctrl-d": "C-d",
	"ctrl-r": "C-r",
	"ctrl-u": "C-u",
	"space":  "Space",
	"bspace": "BSpace",
	"pgup":   "PageUp",
	"pgdn":   "PageDown",
}

func sessionKeys(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	key := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("key")))
	tmuxKey, ok := allowedKeys[key]
	if !ok {
		writeJSON(w, 400, map[string]any{"error": "unknown key " + key})
		return
	}

	res, err := client.Call(map[string]any{"type": "session:get", "sessionId": id}, "session:get", 15*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	sm, _ := res["session"].(map[string]any)
	target, _ := sm["tmuxSession"].(string)
	if target == "" {
		writeJSON(w, 400, map[string]any{"error": "session has no tmux pane"})
		return
	}
	if out, err := exec.Command("tmux", "send-keys", "-t", target, tmuxKey).CombinedOutput(); err != nil { //nolint:gosec // allowlisted key, fixed argv
		writeJSON(w, 502, map[string]any{"error": strings.TrimSpace(string(out))})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "key": tmuxKey})
}
