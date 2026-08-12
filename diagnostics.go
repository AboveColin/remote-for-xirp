package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The daemon exposes its own diagnostics, and three of them are load-bearing here
// rather than decorative:
//
//   - tmux:status lists the panes that actually exist, which is what decides whether
//     the terminal view has anything to show and whether a message can land. Measured:
//     when a pane is killed the daemon notices and marks the session completed within
//     about three seconds, so a live session with a dead pane is a narrow window rather
//     than a standing condition — but it does happen, and while it lasts
//     `session:message` reports success and goes nowhere, because it is
//     fire-and-forget. `hasPane` is also the honest answer for sessions that never had
//     a pane at all.
//   - modules:list says which features this edition actually has. Search is a module;
//     offering a search box that can only ever return nothing is worse than not
//     offering one.
//   - log-viewer:snapshot is the daemon's own log, which is where the real reason for
//     a failure appears. Everything else can only report that something failed.

type tmuxState struct {
	Available bool
	Panes     map[string]bool
	Orphaned  int
	At        time.Time
}

var (
	tmuxMu    sync.Mutex
	tmuxCache tmuxState
)

// tmuxStatus is cached briefly: the session list is polled every few seconds and the
// set of panes does not change that fast.
func tmuxStatus() tmuxState { return tmuxStatusFresh(false) }

// tmuxStatusFresh re-reads when asked. A cached list is briefly wrong just after a
// session is created — the pane exists but the cache predates it — and reporting
// "no pane" for a healthy new session is worse than one extra call.
func tmuxStatusFresh(force bool) tmuxState {
	tmuxMu.Lock()
	defer tmuxMu.Unlock()
	if !force && time.Since(tmuxCache.At) < 3*time.Second && tmuxCache.Panes != nil {
		return tmuxCache
	}
	state := tmuxState{Panes: map[string]bool{}, At: time.Now()}
	if res, err := client.Call(map[string]any{"type": "tmux:status"}, "tmux:status", 10*time.Second); err == nil {
		state.Available, _ = res["available"].(bool)
		if list, ok := res["sessions"].([]any); ok {
			for _, s := range list {
				if sm, ok := s.(map[string]any); ok {
					if id, _ := sm["sessionId"].(string); id != "" {
						state.Panes[id] = true
					}
				}
			}
		}
	}
	if res, err := client.Call(map[string]any{"type": "tmux:orphaned-sessions"}, "tmux:orphaned-sessions", 10*time.Second); err == nil {
		if list, ok := res["sessions"].([]any); ok {
			state.Orphaned = len(list)
		}
	}
	tmuxCache = state
	return state
}

// activeModules is cached for longer; modules are fixed for the life of the app.
var (
	modulesMu   sync.Mutex
	modulesList []string
	modulesAt   time.Time
)

func activeModules() []string {
	modulesMu.Lock()
	defer modulesMu.Unlock()
	if time.Since(modulesAt) < 5*time.Minute && modulesList != nil {
		return modulesList
	}
	out := []string{}
	if res, err := client.Call(map[string]any{"type": "modules:list"}, "modules:list", 10*time.Second); err == nil {
		if list, ok := res["modules"].([]any); ok {
			for _, m := range list {
				if mm, ok := m.(map[string]any); ok {
					if id, _ := mm["id"].(string); id != "" {
						out = append(out, id)
					}
				}
			}
		}
	}
	sort.Strings(out)
	modulesList, modulesAt = out, time.Now()
	return out
}

func handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}

	creds, err := discover()
	if err != nil {
		out["daemon"] = map[string]any{"reachable": false, "error": err.Error()}
	} else {
		out["daemon"] = map[string]any{"reachable": true, "port": creds.Port}
	}

	tm := tmuxStatus()
	out["tmux"] = map[string]any{"available": tm.Available, "panes": len(tm.Panes), "orphaned": tm.Orphaned}
	out["modules"] = activeModules()

	if res, err := client.Call(map[string]any{"type": "db:query-stats"}, "db:query-stats", 10*time.Second); err == nil {
		if stats, ok := res["stats"].(map[string]any); ok {
			// The full stats block carries per-method averages and a table histogram.
			// A phone wants the rate, the busiest tables and the p95, nothing else.
			top := []map[string]any{}
			if byTable, ok := stats["byTable"].(map[string]any); ok {
				type kv struct {
					Table string
					N     float64
				}
				var rows []kv
				for k, v := range byTable {
					if n, ok := v.(float64); ok {
						rows = append(rows, kv{k, n})
					}
				}
				sort.Slice(rows, func(i, j int) bool { return rows[i].N > rows[j].N })
				for i, row := range rows {
					if i == 5 {
						break
					}
					top = append(top, map[string]any{"table": row.Table, "queries": row.N})
				}
			}
			out["db"] = map[string]any{
				"callsPerMinute": stats["callsPerMinute"],
				"p95Duration":    stats["p95Duration"],
				"totalCalls":     stats["totalCalls"],
				"topTables":      top,
			}
		}
	}

	if res, err := client.Call(map[string]any{"type": "update:check"}, "update:status", 15*time.Second); err == nil {
		out["update"] = project(res, []string{"available", "updateCount", "disabled", "lastChecked"})
	}

	writeJSON(w, 200, out)
}

// handleLogs returns the daemon's recent log records. Levels follow pino: 20 debug,
// 30 info, 40 warn, 50 error.
func handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 60
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	minLevel := 30
	if q := r.URL.Query().Get("level"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			minLevel = n
		}
	}

	// Ask for the whole buffer, not `limit` records. Warnings are rare — measured 6
	// warnings and 2 errors in a 1000-record window that was 941 debug lines — so
	// filtering a small window finds nothing and reads as "no problems".
	res, err := client.Call(map[string]any{
		"type": "log-viewer:snapshot", "limit": 1000,
	}, "log-viewer:records", 20*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	records, _ := res["records"].([]any)
	out := []map[string]any{}
	for _, rec := range records {
		rm, ok := rec.(map[string]any)
		if !ok {
			continue
		}
		lvl, _ := rm["level"].(float64)
		if int(lvl) < minLevel {
			continue
		}
		msg, _ := rm["msg"].(string)
		// Log lines are long and full of absolute paths; the head carries the meaning.
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		out = append(out, map[string]any{
			"level": int(lvl),
			"time":  rm["time"],
			"src":   rm["src"],
			"msg":   strings.TrimSpace(msg),
		})
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, 200, map[string]any{"records": out, "minLevel": minLevel})
}

// handleAssetLinks publishes the Digital Asset Links file for an Android wrapper, if one
// has been put in place. It is intentionally unauthenticated: the verification happens
// before any user is involved, so requiring a key would guarantee it fails.
//
// Nothing is generated here. The file contains a signing-certificate fingerprint, which
// only whoever holds the keystore can produce.
func handleAssetLinks(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, "Library", "Application Support", "xirp-remote", "assetlinks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		// A plain 404 is the right answer: no wrapper has been set up.
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}
