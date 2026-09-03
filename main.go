package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

const cookieName = "xr_key"

// toolTextCap bounds how much of a tool call or result is sent to the phone.
//
// Measured on a real session (`?limit=40`, 31 entries after empty reasoning
// entries are dropped): the payload was 25.2 KB, of which 18.7 KB was tool text
// and only 0.4 KB was prose, the largest single entry being 2.9 KB. At 400 the
// same request returns 14.6 KB with 13 entries truncated, and every tool line
// still shows its command and the start of its output.
const toolTextCap = 400

// Set at build time: -ldflags "-X main.version=v0.1.0". A release binary that cannot
// say what it is makes every bug report start with a guess.
var version = "dev"

var (
	client    = NewClient()
	remoteKey string
)

func main() {
	if handled, err := runCLI(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	remoteKey = os.Getenv("XIRP_REMOTE_KEY")
	addr := os.Getenv("XIRP_REMOTE_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	switch {
	case remoteKey == "":
		// Open mode, by explicit choice. In that mode the network is the whole
		// boundary, and anyone who can reach the address can type into agent
		// sessions, which is code execution as this user. So the warning names the
		// address it listens on: a bind of 127.0.0.1 reaches nobody else, and
		// 0.0.0.0 reaches everything that can route to this machine.
		log.Printf("XIRP_REMOTE_KEY is unset: running OPEN on %s, no authentication. Anyone who can reach that address can drive your agent sessions.", addr)
	case len(remoteKey) < 16:
		// A short key is worse than none: it looks like protection while being
		// guessable, so fail loudly rather than half-protect.
		log.Fatal("XIRP_REMOTE_KEY is set but shorter than 16 characters; use a longer key or unset it for open mode")
	default:
		log.Print("authentication enabled")
	}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	static, err := newStaticServer(sub)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	// Cross-origin access exists so one installed app can talk to several hosts.
	// It is enabled only when a key is required: echoing an origin on an open
	// instance would let any web page you visit read and drive your sessions.
	handler := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if remoteKey != "" {
				if origin := r.Header.Get("Origin"); origin != "" {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Xirp-Key")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/api/auth", handleAuth)
	mux.Handle("/api/meta", authed(http.HandlerFunc(handleMeta)))
	mux.Handle("/api/search", authed(http.HandlerFunc(handleSearch)))
	mux.Handle("/api/models", authed(http.HandlerFunc(handleModels)))
	mux.Handle("/api/pair", authed(http.HandlerFunc(handlePair)))
	mux.Handle("/api/diagnostics", authed(http.HandlerFunc(handleDiagnostics)))
	mux.Handle("/api/restorable", authed(http.HandlerFunc(handleRestorable)))
	mux.Handle("/api/restore", authed(http.HandlerFunc(handleRestore)))
	mux.Handle("/api/push/key", authed(http.HandlerFunc(handlePushKey)))
	mux.Handle("/api/push/subscribe", authed(http.HandlerFunc(handlePushSubscribe)))
	mux.Handle("/api/push/unsubscribe", authed(http.HandlerFunc(handlePushUnsubscribe)))
	mux.Handle("/api/push/test", authed(http.HandlerFunc(handlePushTest)))
	mux.Handle("/api/logs", authed(http.HandlerFunc(handleLogs)))
	mux.Handle("/api/prompts", authed(http.HandlerFunc(handlePrompts)))
	mux.Handle("/api/events", authed(http.HandlerFunc(handleEvents)))
	mux.Handle("/api/sessions", authed(http.HandlerFunc(handleSessions)))
	mux.Handle("/api/sessions/", authed(http.HandlerFunc(handleSession)))
	mux.Handle("/api/permissions", authed(http.HandlerFunc(handlePermissions)))
	mux.Handle("/api/permissions/", authed(http.HandlerFunc(handlePermissionRespond)))
	// An Android TWA verifies the origin it wraps by fetching this file from it. Since
	// this app *is* that origin, it serves the file, rather than requiring a separate
	// web server in front just to publish 22 lines of JSON.
	mux.HandleFunc("/.well-known/assetlinks.json", handleAssetLinks)
	mux.Handle("/", static)

	// One socket follows the daemon's broadcasts into the store, which answers the
	// session list, the projects and the live permission requests without asking.
	watchDaemon()

	if err := loadPush(); err != nil {
		log.Printf("could not read push subscriptions: %v", err)
	}
	// The watcher only talks to the daemon when something is subscribed, so this is
	// free until notifications are switched on.
	startPushWatcher()

	log.Printf("xirp-remote %s listening on %s", version, addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// ---- auth ----

func hasKey(r *http.Request) bool {
	if remoteKey == "" {
		return true // open mode
	}
	if c, err := r.Cookie(cookieName); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(remoteKey)) == 1 {
			return true
		}
	}
	if h := r.Header.Get("X-Xirp-Key"); h != "" {
		return subtle.ConstantTimeCompare([]byte(h), []byte(remoteKey)) == 1
	}
	return false
}

func authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasKey(r) {
			writeJSON(w, 401, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	if remoteKey == "" {
		writeJSON(w, 200, map[string]any{"ok": true, "open": true})
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad body"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Key), []byte(remoteKey)) != 1 {
		time.Sleep(400 * time.Millisecond) // blunt the speed of guessing
		writeJSON(w, 401, map[string]any{"error": "wrong key"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    remoteKey,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   60 * 60 * 24 * 30,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- sessions ----

// Only these fields leave the daemon. The full session record carries ~50 keys
// including internal ids and launch options; a phone needs none of that, and a
// narrow projection keeps the list response small on a mobile connection.
var sessionFields = []string{
	"id", "name", "isCustomName", "goal", "status", "workflowStatus",
	"waitingReason", "waitingSince", "model", "requestedModel", "currentAgent",
	"projectId", "branch", "worktreePath", "contextTokens", "contextWindowSize",
	"totalCostUsd", "inputTokens", "outputTokens", "lastActivityAt", "lastMessageAt",
	"createdAt", "completedAt", "lastUserMessage", "parentSessionId", "tags",
	"cliSessionId", "originalCliSessionId",
}

func project(src map[string]any, fields []string) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := src[f]; ok && v != nil {
			out[f] = v
		}
	}
	return out
}

// sessionRow reads one session. The store holds every session the daemon lists, so this
// usually costs nothing; a session the list does not carry, which search can surface,
// still costs one call.
func sessionRow(id string) (map[string]any, error) {
	if row := live.session(id); row != nil {
		return row, nil
	}
	res, err := client.Call(map[string]any{"type": "session:get", "sessionId": id}, "session:get", 15*time.Second)
	if err != nil {
		return nil, err
	}
	row, _ := res["session"].(map[string]any)
	if row == nil {
		return nil, errors.New("session not found")
	}
	return row, nil
}

// Project names and default branches come from the store, which the daemon's
// project:added, project:updated and project:removed broadcasts keep current.
func projectNames() map[string]string {
	out := map[string]string{}
	for _, row := range live.projectRows() {
		if id, _ := row["id"].(string); id != "" {
			out[id], _ = row["name"].(string)
		}
	}
	return out
}

func projectBase(projectID string) string {
	for _, row := range live.projectRows() {
		if id, _ := row["id"].(string); id == projectID {
			base, _ := row["defaultBranch"].(string)
			return base
		}
	}
	return ""
}

// handleSessions serves GET (list) and POST (create) on /api/sessions.
func handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		handleCreate(w, r)
		return
	}
	raw := live.sessionRows()
	names := projectNames()
	tm := tmuxStatus()
	forcedFresh := false
	out := make([]map[string]any, 0, len(raw))
	for _, sm := range raw {
		p := project(sm, sessionFields)
		if id, ok := sm["projectId"].(string); ok {
			p["projectName"] = names[id]
		}
		// A session can be `running` in the database with no pane behind it. Only the
		// tmux side knows, and without this the terminal view shows nothing and a
		// message is accepted and dropped.
		if id, ok := sm["id"].(string); ok && tm.Available {
			if tmuxName, _ := sm["tmuxSession"].(string); tmuxName != "" {
				if !tm.Panes[id] && !forcedFresh {
					// Confirm before reporting a missing pane: the cached list can
					// predate a session that was just created. Once per request. A
					// forced read bypasses the cache, so doing it per paneless session
					// meant two daemon calls each, every five seconds.
					tm = tmuxStatusFresh(true)
					forcedFresh = true
				}
				p["hasPane"] = tm.Panes[id]
			}
		}
		out = append(out, p)
	}
	writeJSON(w, 200, map[string]any{"sessions": out, "modules": activeModules()})
}

// handleSession serves /api/sessions/{id} and /api/sessions/{id}/{action}.
func handleSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		writeJSON(w, 404, map[string]any{"error": "no session id"})
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		sessionDetail(w, r, id)
	case "message":
		sessionMessage(w, r, id)
	case "stop":
		sessionStop(w, r, id)
	case "git":
		sessionGit(w, r, id)
	case "urls":
		sessionURLs(w, r, id)
	case "log":
		sessionLog(w, r, id)
	case "ack":
		sessionAck(w, r, id)
	case "pane":
		sessionPane(w, r, id)
	case "keys":
		sessionKeys(w, r, id)
	case "changes":
		sessionChanges(w, r, id)
	case "diff":
		sessionFileDiff(w, r, id)
	case "fork":
		sessionFork(w, r, id)
	case "swap":
		sessionSwapAgent(w, r, id)
	case "rename":
		sessionRename(w, r, id)
	case "file":
		sessionFile(w, r, id)
	default:
		writeJSON(w, 404, map[string]any{"error": "unknown action " + action})
	}
}

func sessionDetail(w http.ResponseWriter, r *http.Request, id string) {
	sm, err := sessionRow(id)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	out := project(sm, sessionFields)
	if pid, ok := sm["projectId"].(string); ok {
		out["projectName"] = projectNames()[pid]
	}

	limit := 30
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	payload := map[string]any{"session": out}
	// The terminal view draws the tmux pane, not the transcript, so its poll asks for
	// the session alone. Parsing a transcript means spawning `node squab session-parse`,
	// measured at 0.07 s and 3.5 MB of JSON for a 1111-message session, every four
	// seconds, for something nothing displays.
	if r.URL.Query().Get("transcript") == "0" {
		payload["transcriptSkipped"] = true
		writeJSON(w, 200, payload)
		return
	}
	if parsed, err := client.ParseSession(id, limit); err == nil {
		payload["messages"] = transcript(parsed)
		payload["messageCount"] = parsed["messageCount"]
		payload["totalMessages"] = parsed["totalMessages"]
		payload["truncatedFromStart"] = parsed["truncatedFromStart"]
	} else {
		// A missing transcript is not a failed request: fresh sessions and
		// terminal-only sessions have none. Report why instead of erroring.
		payload["messages"] = []any{}
		payload["transcriptError"] = err.Error()
	}
	writeJSON(w, 200, payload)
}

// transcript flattens squab's parsed messages to what the UI renders, dropping
// the `raw` harness payload (it is by far the largest part of the response).
func transcript(parsed map[string]any) []map[string]any {
	msgs, _ := parsed["messages"].([]any)
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		// Reasoning entries arrive with an empty text field (the harness does not
		// persist their content), so they would render as blank bubbles.
		text, _ := mm["text"].(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		entry := project(mm, []string{"id", "ts", "role", "type", "text"})
		// Tool calls and their results are most of a transcript by volume and a
		// single file dump can be tens of thousands of characters, which on a
		// phone means scrolling past one tool result for a screenful of thumb
		// swipes. The head of the text says what the tool did, which is what this
		// view is for; the desktop app is where you read the whole thing.
		if t, _ := mm["type"].(string); t == "tool_use" || t == "tool_result" {
			if len(text) > toolTextCap {
				entry["text"] = text[:toolTextCap] + "\n… truncated, " + strconv.Itoa(len(text)) + " chars total"
				entry["truncated"] = true
			}
		}
		out = append(out, entry)
	}
	return out
}

func sessionMessage(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Text string `json:"text"`
		// Enter distinguishes "send it and let the agent run" from "leave it in the
		// agent's input". Absent means send, which is what a plain client expects.
		Enter *bool `json:"enter"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad body"})
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, 400, map[string]any{"error": "empty text"})
		return
	}
	// session:message types the text into the agent and submits it. It is the
	// call that actually drives a session; message:send only writes a row.
	enter := true
	if body.Enter != nil {
		enter = *body.Enter
	}
	err := client.Fire(map[string]any{
		"type":      "session:message",
		"sessionId": id,
		"text":      body.Text,
		"enter":     enter,
	})
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("sent message to session %s (%d chars, enter=%v)", id, len(body.Text), enter)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func sessionStop(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	res, err := client.Call(map[string]any{"type": "session:stop", "sessionId": id}, "session:updated", 15*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("stopped session %s", id)
	status := ""
	if sm, ok := res["session"].(map[string]any); ok {
		status, _ = sm["status"].(string)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": status})
}

// ---- permission requests ----

// handlePermissions answers from the store. The daemon broadcasts permission:request the
// moment one appears, which is the only way to see one at all: it holds each for about
// 500 ms, so a poll asking for the list arrives after every one of them has gone.
func handlePermissions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"requests": live.permissionRows()})
}

func handlePermissionRespond(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	requestID := strings.TrimPrefix(r.URL.Path, "/api/permissions/")
	if requestID == "" {
		writeJSON(w, 404, map[string]any{"error": "no request id"})
		return
	}
	var body struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad body"})
		return
	}
	if body.Behavior != "allow" && body.Behavior != "deny" {
		writeJSON(w, 400, map[string]any{"error": "behavior must be allow or deny"})
		return
	}
	req := map[string]any{"type": "permission:respond", "requestId": requestID, "behavior": body.Behavior}
	if body.Message != "" {
		req["message"] = body.Message
	}
	// A request that is still open resolves at once. For one that has expired the
	// daemon logs a debug line and sends nothing, so every extra second here is spent
	// waiting for an answer that is never coming. It holds a request for
	// Math.min(timeout, 500) ms, so expired is the normal case from a phone.
	if _, err := client.Call(req, "permission:resolved", 3*time.Second); err != nil {
		var te timeoutError
		if errors.As(err, &te) {
			writeJSON(w, 409, map[string]any{
				"error": "that request had already expired: Xirp holds a permission request for about half a second before the agent's own dialog takes over",
			})
			return
		}
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("permission %s -> %s", requestID, body.Behavior)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
