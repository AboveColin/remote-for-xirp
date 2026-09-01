package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---- search ----

// handleSearch runs the daemon's full-text search across session metadata,
// messages and JSONL transcripts.
//
// Results stream in per source and can repeat the same session, so they are
// deduplicated here — the phone gets one row per session, which is what a person
// wants to tap on.
func handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, 400, map[string]any{"error": "missing q"})
		return
	}
	searchID := fmt.Sprintf("xr-%d", time.Now().UnixNano())
	frames, err := client.CallStream(map[string]any{
		"type":     "session-search:search",
		"query":    q,
		"searchId": searchID,
	}, "session-search:results", 12*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}

	seen := map[string]bool{}
	out := []map[string]any{}
	for _, f := range frames {
		if id, _ := f["searchId"].(string); id != searchID {
			continue // a stale search's frames, not ours
		}
		list, _ := f["results"].([]any)
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			sid, _ := m["sessionId"].(string)
			if sid == "" || seen[sid] {
				continue
			}
			seen[sid] = true
			out = append(out, project(m, []string{
				"sessionId", "name", "status", "snippet", "matchField",
				"lastActivityAt", "createdAt", "provider",
			}))
			if len(out) >= 40 {
				break
			}
		}
	}
	writeJSON(w, 200, map[string]any{"query": q, "results": out, "frames": len(frames)})
}

// ---- metadata for the create form ----

type metaCache struct {
	payload map[string]any
	at      time.Time
}

var (
	metaMu sync.Mutex
	mcache metaCache
)

// handleMeta returns the projects and coding agents a new session can be created
// with. Both change rarely, so the answer is cached for a minute; the create
// sheet asks for it every time it opens.
func handleMeta(w http.ResponseWriter, r *http.Request) {
	metaMu.Lock()
	cached := mcache
	metaMu.Unlock()
	if time.Since(cached.at) < 60*time.Second && cached.payload != nil {
		writeJSON(w, 200, cached.payload)
		return
	}

	projects := []map[string]any{}
	if res, err := client.Call(map[string]any{"type": "projects:list"}, "projects:list", 10*time.Second); err == nil {
		if list, ok := res["projects"].([]any); ok {
			for _, p := range list {
				if pm, ok := p.(map[string]any); ok {
					projects = append(projects, project(pm, []string{
						"id", "name", "path", "defaultBranch", "activeSessions", "lastActivityAt",
					}))
				}
			}
		}
	}

	agents := []map[string]any{}
	if res, err := client.Call(map[string]any{"type": "agents:list"}, "agents:list", 10*time.Second); err == nil {
		// The reply to `agents:list` carries them under `harnesses`, not `agents`.
		// Reading the obvious key returned an empty list with no error.
		list, ok := res["harnesses"].([]any)
		if !ok {
			list, ok = res["agents"].([]any)
		}
		if ok {
			for _, a := range list {
				am, ok := a.(map[string]any)
				if !ok {
					continue
				}
				// Only agents actually installed can start a session; offering the
				// rest would produce a create that fails after the fact.
				if installed, _ := am["installed"].(bool); !installed {
					continue
				}
				agents = append(agents, project(am, []string{"agentName", "version", "description"}))
			}
		}
	}

	payload := map[string]any{"projects": projects, "agents": agents}
	metaMu.Lock()
	mcache = metaCache{payload: payload, at: time.Now()}
	metaMu.Unlock()
	writeJSON(w, 200, payload)
}

// ---- models ----

// handleModels lists the models an agent can run, with context window and price.
// The daemon knows these per harness, so the create sheet can offer "cheap model
// for a small job" without anyone memorising model ids.
func handleModels(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		writeJSON(w, 400, map[string]any{"error": "missing agent"})
		return
	}
	res, err := client.Call(map[string]any{
		"type": "settings:getModels", "agentName": agent,
	}, "settings:models", 15*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	list, _ := res["models"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		entry := project(mm, []string{"model", "contextWindowSize"})
		// Input price is the number that decides "is this model worth it for this
		// errand". The rest of the pricing block is noise on a phone.
		if p, ok := mm["pricePerMillion"].(map[string]any); ok {
			entry["inputPerMillion"] = p["input"]
			entry["outputPerMillion"] = p["output"]
		}
		out = append(out, entry)
	}
	writeJSON(w, 200, map[string]any{"agent": agent, "models": out})
}

// ---- session extras ----

// sessionURLs returns URLs the daemon spotted in the session's terminal output —
// dev server addresses, preview links, PR links. On a phone these are the payoff:
// an agent says "running on :3000" and you can just tap it.
func sessionURLs(w http.ResponseWriter, r *http.Request, id string) {
	res, err := client.Call(map[string]any{"type": "session:urls:get", "sessionId": id}, "session:urls", 15*time.Second)
	if err != nil {
		writeJSON(w, 200, map[string]any{"urls": []any{}, "unavailable": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"urls": res["urls"]})
}

// sessionLog returns recent commits from the session's worktree, so you can see
// what an agent actually committed rather than inferring it from the transcript.
func sessionLog(w http.ResponseWriter, r *http.Request, id string) {
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
	projectID, _ := sm["projectId"].(string)
	if projectID == "" {
		writeJSON(w, 200, map[string]any{"commits": []any{}, "unavailable": "session has no project"})
		return
	}
	req := map[string]any{"type": "git:log", "projectId": projectID, "limit": 10}
	if wt, _ := sm["worktreePath"].(string); wt != "" {
		req["worktreePath"] = wt
	}
	if br, _ := sm["branch"].(string); br != "" {
		req["branch"] = br
	}
	gres, err := client.Call(req, "git:log", 25*time.Second, "git:error")
	if err != nil {
		writeJSON(w, 200, map[string]any{"commits": []any{}, "unavailable": err.Error()})
		return
	}
	commits, _ := gres["commits"].([]any)
	out := make([]map[string]any, 0, len(commits))
	for _, c := range commits {
		if cm, ok := c.(map[string]any); ok {
			out = append(out, project(cm, []string{"hash", "shortHash", "subject", "message", "author", "date", "relativeDate"}))
		}
	}
	writeJSON(w, 200, map[string]any{"commits": out})
}

// sessionAck clears the "needs attention" state on a finished or failed session.
// It is the one write here that cannot break anything.
func sessionAck(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	if _, err := client.Call(map[string]any{"type": "session:acknowledge", "sessionId": id}, "session:updated", 15*time.Second); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- create ----

// handleCreate starts a new session. The daemon answers `session:creating` first
// and `session:created` when the worktree and agent are ready, which can take a
// few seconds, so this waits for the latter.
func handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		ProjectID   string `json:"projectId"`
		Goal        string `json:"goal"`
		Name        string `json:"name"`
		Agent       string `json:"agent"`
		Model       string `json:"model"`
		NewBranch   string `json:"newBranch"`
		UseTerminal bool   `json:"useTerminal"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad body"})
		return
	}
	if body.ProjectID == "" {
		writeJSON(w, 400, map[string]any{"error": "projectId required"})
		return
	}

	// A plain shell is a different message entirely. `session:create`'s useTerminal
	// flag does NOT mean "terminal instead of agent" — it selects the tmux transport
	// (the default), and setting it to false selects Claude-only SDK mode. Passing it
	// hoping for a shell produces a full agent session, which is what happened here
	// before this was checked: the pane came up running Claude Code.
	if body.UseTerminal {
		res, err := client.Call(map[string]any{
			"type": "project:create-terminal", "projectId": body.ProjectID,
		}, "session:created", 60*time.Second)
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": err.Error()})
			return
		}
		sm, _ := res["session"].(map[string]any)
		id, _ := sm["id"].(string)
		log.Printf("created terminal session %s (project=%s)", id, body.ProjectID)
		writeJSON(w, 200, map[string]any{"ok": true, "session": project(sm, sessionFields)})
		return
	}

	if strings.TrimSpace(body.Goal) == "" {
		writeJSON(w, 400, map[string]any{"error": "goal required for an agent session"})
		return
	}

	req := map[string]any{"type": "session:create", "projectId": body.ProjectID}
	if body.Goal != "" {
		req["goal"] = body.Goal
	}
	if body.Name != "" {
		req["name"] = body.Name
	}
	if body.NewBranch != "" {
		req["newBranch"] = body.NewBranch
	}
	if body.Agent != "" {
		agent := map[string]any{"agentName": body.Agent}
		if body.Model != "" {
			// The model is not a top-level field: the daemon derives the session's
			// requestedModel from agent.options.model, checked in its create handler.
			agent["options"] = map[string]any{"model": body.Model}
		}
		req["agent"] = agent
	}

	// Worktree creation plus agent startup is slow enough that the default
	// timeout would report failure on a session that is in fact being born.
	res, err := client.Call(req, "session:created", 90*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	sm, _ := res["session"].(map[string]any)
	id, _ := sm["id"].(string)
	log.Printf("created session %s (project=%s agent=%s terminal=%v)", id, body.ProjectID, body.Agent, body.UseTerminal)
	writeJSON(w, 200, map[string]any{"ok": true, "session": project(sm, sessionFields)})
}

// ---- git ----

// sessionGit reports the working tree state for a session's checkout: branch plus
// a count of changed files, so you can see from a phone whether an agent has left
// work uncommitted.
func sessionGit(w http.ResponseWriter, r *http.Request, id string) {
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
	projectID, _ := sm["projectId"].(string)
	worktree, _ := sm["worktreePath"].(string)
	if projectID == "" {
		writeJSON(w, 200, map[string]any{"unavailable": "session has no project"})
		return
	}

	req := map[string]any{"type": "git:status", "projectId": projectID}
	if worktree != "" {
		req["worktreePath"] = worktree
	}
	// A session whose worktree was deleted answers git:error, not the generic error
	// frame, so this call names that type. Without it the call waits out its timeout.
	gres, err := client.Call(req, "git:status", 20*time.Second, "git:error")
	if err != nil {
		writeJSON(w, 200, map[string]any{"unavailable": err.Error()})
		return
	}
	status, _ := gres["status"].(map[string]any)
	files, _ := status["files"].([]any)

	var staged, modified, untracked int
	for _, f := range files {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := fm["staged"].(bool); s {
			staged++
			continue
		}
		if st, _ := fm["status"].(string); st == "?" {
			untracked++
		} else {
			modified++
		}
	}
	branch, _ := status["branch"].(string)
	writeJSON(w, 200, map[string]any{
		"branch":    branch,
		"staged":    staged,
		"modified":  modified,
		"untracked": untracked,
		"clean":     len(files) == 0,
		"total":     len(files),
	})
}

// ---- fork and hand over ----
//
// Both need the CLI session id, not just the session id: the daemon forks the agent's
// own conversation, which it tracks separately from the session row. `sessions:list`
// carries it as `cliSessionId`, so it is read from the session rather than asked for.

func cliSessionID(sm map[string]any) string {
	if v, _ := sm["cliSessionId"].(string); v != "" {
		return v
	}
	// A resumed session keeps its first CLI id here; either identifies the transcript.
	v, _ := sm["originalCliSessionId"].(string)
	return v
}

// sessionFork branches a conversation. The usual reason is that an agent went down the
// wrong path: fork from where it was still right, and leave the original alone.
func sessionFork(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		NewBranch string `json:"newBranch"`
		Worktree  bool   `json:"forkIntoWorktree"`
		Agent     string `json:"agent"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)

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
	cli := cliSessionID(sm)
	if cli == "" {
		writeJSON(w, 400, map[string]any{"error": "this session has no CLI conversation to fork"})
		return
	}

	req := map[string]any{"type": "session:fork", "sessionId": id, "cliSessionId": cli}
	if pid, _ := sm["projectId"].(string); pid != "" {
		req["projectId"] = pid
	}
	if body.NewBranch != "" {
		req["newBranch"] = body.NewBranch
		req["forkIntoWorktree"] = true
	} else if body.Worktree {
		req["forkIntoWorktree"] = true
	}
	if body.Agent != "" {
		req["agent"] = map[string]any{"agentName": body.Agent}
	}

	// A fork can create a worktree, which is slow enough that the default timeout
	// would report failure on a fork that is still being set up.
	fres, err := client.Call(req, "session:created", 90*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	nsm, _ := fres["session"].(map[string]any)
	nid, _ := nsm["id"].(string)
	log.Printf("forked session %s -> %s (branch=%q agent=%q)", id, nid, body.NewBranch, body.Agent)
	writeJSON(w, 200, map[string]any{"ok": true, "session": project(nsm, sessionFields)})
}

// sessionSwapAgent hands a session to a different agent, keeping the conversation.
func sessionSwapAgent(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Agent  string `json:"agent"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil || body.Agent == "" {
		writeJSON(w, 400, map[string]any{"error": "agent required"})
		return
	}

	req := map[string]any{"type": "session:swap-agent", "sessionId": id, "agentName": body.Agent}
	if body.Reason != "" {
		req["reason"] = body.Reason
	}
	// Every way a swap can fail answers session:swap-agent:error, never the generic
	// error frame, and the frame carries the only text worth showing: a code, a
	// sentence and sometimes a hint. Xirp 0.22.0 added UNSUPPORTED_AGENT_VERSION here
	// for an agent CLI older than the version Xirp tests against, and
	// ORCHESTRATOR_NOT_FOUND arrives with "restart the session to enable it".
	res, err := client.Call(req, "session:agent-swapped", 60*time.Second, "session:swap-agent:error")
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	from, _ := res["from"].(string)
	to, _ := res["to"].(string)
	if to == "" {
		to = body.Agent
	}
	log.Printf("swapped session %s from %s to %s", id, from, to)

	out := map[string]any{"ok": true, "agent": to, "from": from, "to": to}
	// session:agent-swapped carries sessionId, from and to, and no session object. The
	// daemon broadcasts session:updated with the row just before it, but a call that
	// waits for one type discards the other, so the row is read back here.
	if got, err := client.Call(map[string]any{"type": "session:get", "sessionId": id}, "session:get", 15*time.Second); err == nil {
		if sm, ok := got["session"].(map[string]any); ok {
			out["session"] = project(sm, sessionFields)
		}
	}
	writeJSON(w, 200, out)
}

// ---- restoring sessions after the app restarts ----
//
// When Xirp restarts, sessions it was running are left needing a decision: bring them
// back, or dismiss them. Until now that decision could only be made at the desk, which
// is exactly the wrong place if you are away and an agent has stopped.

func handleRestorable(w http.ResponseWriter, r *http.Request) {
	res, err := client.Call(map[string]any{"type": "sessions:restorable"}, "sessions:restorable", 20*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	raw, _ := res["sessions"].([]any)
	names := projectNames()
	out := make([]map[string]any, 0, len(raw))
	for _, s := range raw {
		if sm, ok := s.(map[string]any); ok {
			e := project(sm, sessionFields)
			if pid, _ := sm["projectId"].(string); pid != "" {
				e["projectName"] = names[pid]
			}
			out = append(out, e)
		}
	}
	writeJSON(w, 200, map[string]any{"sessions": out})
}

// handleRestore revives or dismisses sessions. The daemon reports progress and then a
// completion frame, so this collects until the completion arrives.
func handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Restore []string `json:"restore"`
		Dismiss []string `json:"dismiss"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad body"})
		return
	}
	if len(body.Restore) == 0 && len(body.Dismiss) == 0 {
		writeJSON(w, 400, map[string]any{"error": "nothing to restore or dismiss"})
		return
	}
	// Both lists are required by the daemon, even when empty.
	if body.Restore == nil {
		body.Restore = []string{}
	}
	if body.Dismiss == nil {
		body.Dismiss = []string{}
	}

	// Restoring starts agents, which takes as long as it takes; the completion frame is
	// what says it finished.
	res, err := client.Call(map[string]any{
		"type": "sessions:restore-bulk", "sessionIds": body.Restore, "dismissIds": body.Dismiss,
	}, "sessions:restore-complete", 120*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("restored %d session(s), dismissed %d", len(body.Restore), len(body.Dismiss))
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"restored":  len(body.Restore),
		"dismissed": len(body.Dismiss),
		"result":    project(res, []string{"restored", "dismissed", "failed", "errors"}),
	})
}

// ---- renaming ----

// sessionRename sets a session's display name, or asks the agent to generate one. A
// session called "Session" among twenty others is the sort of thing that only annoys
// you when you are away from the machine that could fix it.
func sessionRename(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Name       string `json:"name"`
		Regenerate bool   `json:"regenerate"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)

	if body.Regenerate {
		// The daemon answers title-generating first and updated when the new title
		// lands; the second is the one worth waiting for.
		res, err := client.Call(map[string]any{
			"type": "session:regenerate-title", "sessionId": id,
		}, "session:updated", 60*time.Second)
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": err.Error()})
			return
		}
		sm, _ := res["session"].(map[string]any)
		writeJSON(w, 200, map[string]any{"ok": true, "session": project(sm, sessionFields)})
		return
	}

	if strings.TrimSpace(body.Name) == "" {
		writeJSON(w, 400, map[string]any{"error": "name required"})
		return
	}
	res, err := client.Call(map[string]any{
		"type": "session:update", "sessionId": id, "name": body.Name,
	}, "session:updated", 20*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	sm, _ := res["session"].(map[string]any)
	log.Printf("renamed session %s", id)
	writeJSON(w, 200, map[string]any{"ok": true, "session": project(sm, sessionFields)})
}

// ---- reading a file ----

// fileMaxBytes bounds what is sent to the phone. Measured: this project's own CLAUDE.md
// is 23.6 KB, which is a big markdown file by any standard, so 200 KB carries anything
// worth reading on a phone and refuses a bundle or a binary.
const fileMaxBytes = 200000

// fileReadCap refuses a read before it happens. `files:read` has no offset or limit
// parameter, so the daemon loads the whole file and sends all of it, and only then is
// it cut to fileMaxBytes here. 5 MB is 25 times what the phone will ever display and
// far above any file a person reads on one, so only a bundle, a lockfile or a binary
// reaches it. The refusal names the limit and the size.
const fileReadCap = 5 * 1024 * 1024

// statObjection turns a files:stat answer into the reason a path cannot be shown, or ""
// when it can be. Without the stat, a directory and a missing file arrive as the same
// unexplained read failure.
func statObjection(st map[string]any) string {
	if why, _ := st["error"].(string); why != "" {
		return why
	}
	if exists, ok := st["exists"].(bool); ok && !exists {
		return "no such file in this session's checkout"
	}
	if dir, _ := st["isDirectory"].(bool); dir {
		return "that path is a directory, not a file"
	}
	if size, ok := st["size"].(float64); ok && size > fileReadCap {
		return fmt.Sprintf("that file is %.1f MB, over the %d MB this will read",
			size/(1024*1024), fileReadCap/(1024*1024))
	}
	return ""
}

// sessionFile reads one file from the session's checkout, so a file an agent mentioned
// can be read without a laptop.
func sessionFile(w http.ResponseWriter, r *http.Request, id string) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, 400, map[string]any{"error": "missing path"})
		return
	}
	projectID, worktree, _, err := sessionProject(id)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	if projectID == "" {
		writeJSON(w, 404, map[string]any{"error": "session has no project"})
		return
	}
	req := map[string]any{"type": "files:read", "projectId": projectID, "path": path}
	if worktree != "" {
		req["worktreePath"] = worktree
	}

	// files:stat, added in Xirp 0.20.1, classifies a path without reading it. Asking
	// first is what lets this say "that is a directory" or "no such file" instead of
	// passing on a read error, and it keeps a bundle from being read at all.
	statReq := map[string]any{"type": "files:stat", "projectId": projectID, "path": path}
	if worktree != "" {
		statReq["worktreePath"] = worktree
	}
	if st, err := client.Call(statReq, "files:stat", 15*time.Second); err == nil {
		if why := statObjection(st); why != "" {
			writeJSON(w, 200, map[string]any{"path": path, "unavailable": why})
			return
		}
	}

	res, err := client.Call(req, "files:read", 25*time.Second)
	if err != nil {
		writeJSON(w, 200, map[string]any{"unavailable": err.Error()})
		return
	}
	// The files module reports a failed read inside the success frame rather than as an
	// error frame, so this is the only place a bad path shows up. Read as a success it
	// renders as an empty file.
	if why, _ := res["error"].(string); why != "" {
		writeJSON(w, 200, map[string]any{"path": path, "unavailable": why})
		return
	}
	content, _ := res["content"].(string)
	truncated := false
	if len(content) > fileMaxBytes {
		content = content[:fileMaxBytes]
		truncated = true
	}
	writeJSON(w, 200, map[string]any{
		"path":      path,
		"content":   content,
		"size":      res["size"],
		"mtime":     res["mtime"],
		"truncated": truncated,
	})
}
