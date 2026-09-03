package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"
)

// The daemon tells every connected client what changed. It broadcasts session:updated
// from 43 places, plus session:created, session:deleted, project:added, project:updated,
// project:removed, sessions:restorable, permission:request and permission:resolved.
//
// This app used to throw all of that away, because a call reads until it sees the one
// type it asked for and skips the rest. So the phone polled instead: three daemon calls
// every five seconds for a list that had not changed, and an agent that finished took up
// to five seconds to show up.
//
// One socket now follows those broadcasts into a store, and the store answers the
// session list, the project names, the restorable set and the live permission requests
// without asking the daemon anything.
//
// Two things keep it honest. A full resync runs on every connect and every minute after
// that, so a frame lost in a gap cannot leave a stale row for long. And that resync
// counts the rows it had to correct, which /api/diagnostics reports: a drift that is not
// zero in normal use means the rules below are wrong, and polling should come back until
// they are right.

// resyncEvery is the safety net behind the broadcasts, not the main mechanism. A minute
// is short enough that a missed frame is a blip and long enough to be three daemon calls
// an hour rather than 2160.
const resyncEvery = time.Minute

// idleReconnect reconnects the watcher after this long without a single frame. The
// daemon broadcasts a frame per database query, so silence this long means the socket
// died without saying so.
const idleReconnect = 2 * time.Minute

// fillRetry bounds how often a cold store asks again. A daemon that is not running
// fails the dial in milliseconds, but one that is reachable and silent costs a call
// timeout, and paying that on every request would stall the whole app.
const fillRetry = 2 * time.Second

// permissionGrace is how long a request stays answerable. The daemon holds one for
// Math.min(timeout, 500) ms and then its own dialog takes over, so after that the phone
// can still show the prompt but has to answer it through the pane.
const permissionGrace = 500 * time.Millisecond

// change says which part of the store moved. It carries no data: the phone re-reads the
// endpoint it already knows, which now costs the daemon nothing. One kind of event to
// build, one to parse, and a dropped one cannot leave the phone holding half a row.
type change struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

type store struct {
	mu          sync.RWMutex
	sessions    map[string]map[string]any
	projects    map[string]map[string]any
	restorable  []map[string]any
	permissions map[string]map[string]any
	syncedAt    time.Time
	triedAt     time.Time
	drift       int
	subs        map[chan change]bool
}

var live = &store{
	sessions:    map[string]map[string]any{},
	projects:    map[string]map[string]any{},
	permissions: map[string]map[string]any{},
	subs:        map[chan change]bool{},
}

// ---- what the handlers read ----

// sessionRows returns every session the daemon has told us about, filling the store
// first if nothing has landed yet, so the first request after a start behaves like the
// poll it replaced.
func (s *store) sessionRows() []map[string]any {
	s.fill()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.sessions))
	for _, row := range s.sessions {
		out = append(out, row)
	}
	return out
}

func (s *store) session(id string) map[string]any {
	s.fill()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

func (s *store) projectRows() []map[string]any {
	s.fill()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.projects))
	for _, row := range s.projects {
		out = append(out, row)
	}
	return out
}

func (s *store) restorableRows() []map[string]any {
	s.fill()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]map[string]any{}, s.restorable...)
}

// permissionRows returns the live requests, each marked with whether the daemon still
// holds it. Past the grace period it does not, and only the agent's own dialog can
// answer.
func (s *store) permissionRows() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.permissions))
	for _, row := range s.permissions {
		copied := map[string]any{"expired": true}
		for k, v := range row {
			copied[k] = v
		}
		if at, ok := row["seenAt"].(time.Time); ok {
			copied["expired"] = time.Since(at) > permissionGrace
			delete(copied, "seenAt")
		}
		out = append(out, copied)
	}
	return out
}

// health is what diagnostics reports about the store itself.
func (s *store) health() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]any{
		"sessions": len(s.sessions),
		"projects": len(s.projects),
		"drift":    s.drift,
		"watchers": len(s.subs),
	}
	if !s.syncedAt.IsZero() {
		out["syncedSecondsAgo"] = int(time.Since(s.syncedAt).Seconds())
	}
	return out
}

// fill loads the store on first use, so a handler never has to ask whether it is warm.
//
// It gives up quickly and retries later rather than trying on every read: a daemon that
// is reachable but silent would otherwise make each request wait out three full call
// timeouts, one after another.
func (s *store) fill() {
	s.mu.RLock()
	cold := s.syncedAt.IsZero() && time.Since(s.triedAt) > fillRetry
	s.mu.RUnlock()
	if cold {
		s.resync()
	}
}

// ---- following the daemon ----

// watchDaemon follows the broadcasts for the life of the process. It holds a socket of
// its own rather than one from the pool, because it never gives it back.
func watchDaemon() {
	go live.follow()
	go live.resyncLoop()
}

func (s *store) resyncLoop() {
	for range time.Tick(resyncEvery) {
		s.resync()
	}
}

func (s *store) follow() {
	cn := &conn{}
	backoff := time.Second
	for {
		err := s.read(cn)
		cn.drop()
		if err != nil {
			// A daemon that is not running is the normal state when Xirp is quit, so
			// this is not an error worth a line every second.
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

// read consumes broadcasts until the socket fails. It resyncs first, because anything
// that happened while there was no socket was never broadcast to us.
func (s *store) read(cn *conn) error {
	if err := cn.connect(); err != nil {
		return err
	}
	s.resync()
	for {
		if err := cn.ws.SetReadDeadline(time.Now().Add(idleReconnect)); err != nil {
			return err
		}
		_, raw, err := cn.ws.ReadMessage()
		if err != nil {
			return err
		}
		// The type is read on its own first. The daemon broadcasts a frame per database
		// query, and decoding all of those into maps would be the bridge's whole idle
		// cost for frames it discards.
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &head) != nil || !interesting(head.Type) {
			continue
		}
		var msg map[string]any
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if c, ok := s.apply(msg); ok {
			s.publish(c)
		}
	}
}

// interesting names the broadcasts the store keeps. Everything else the daemon says is
// for the desktop app.
func interesting(t string) bool {
	switch t {
	case "session:created", "session:updated", "session:deleted",
		"project:added", "project:updated", "project:removed",
		"sessions:restorable", "permission:request", "permission:resolved":
		return true
	}
	return false
}

// apply folds one broadcast into the store and says what changed.
func (s *store) apply(msg map[string]any) (change, bool) {
	kind, _ := msg["type"].(string)
	s.mu.Lock()
	defer s.mu.Unlock()

	switch kind {
	case "session:created", "session:updated":
		row, _ := msg["session"].(map[string]any)
		id, _ := row["id"].(string)
		if id == "" {
			return change{}, false
		}
		s.sessions[id] = row
		return change{Kind: "session", ID: id}, true

	case "session:deleted":
		id := sessionOf(msg)
		if id == "" {
			return change{}, false
		}
		delete(s.sessions, id)
		return change{Kind: "sessions"}, true

	case "project:added", "project:updated":
		row, _ := msg["project"].(map[string]any)
		id, _ := row["id"].(string)
		if id == "" {
			return change{}, false
		}
		s.projects[id] = row
		return change{Kind: "projects"}, true

	case "project:removed":
		id, _ := msg["projectId"].(string)
		if id == "" {
			return change{}, false
		}
		delete(s.projects, id)
		return change{Kind: "projects"}, true

	case "sessions:restorable":
		s.restorable = rows(msg["sessions"])
		return change{Kind: "restorable"}, true

	case "permission:request":
		id, _ := msg["requestId"].(string)
		if id == "" {
			return change{}, false
		}
		row := map[string]any{"seenAt": time.Now()}
		for k, v := range msg {
			if k != "type" {
				row[k] = v
			}
		}
		row["id"] = id
		s.permissions[id] = row
		return change{Kind: "permissions", ID: sessionOf(msg)}, true

	case "permission:resolved":
		id, _ := msg["requestId"].(string)
		delete(s.permissions, id)
		return change{Kind: "permissions"}, true
	}
	return change{}, false
}

// resync asks for the whole truth and counts what it had to correct. The count is the
// only way anyone would notice the rules in apply going wrong.
func (s *store) resync() {
	s.mu.Lock()
	s.triedAt = time.Now()
	s.mu.Unlock()

	sessions, sErr := listRows("sessions:list", "sessions")
	projects, pErr := listRows("projects:list", "projects")
	restorable, rErr := listRows("sessions:restorable", "sessions")
	if sErr != nil || pErr != nil || rErr != nil {
		return
	}

	fresh := make(map[string]map[string]any, len(sessions))
	for _, row := range sessions {
		if id, _ := row["id"].(string); id != "" {
			fresh[id] = row
		}
	}
	freshProjects := make(map[string]map[string]any, len(projects))
	for _, row := range projects {
		if id, _ := row["id"].(string); id != "" {
			freshProjects[id] = row
		}
	}

	s.mu.Lock()
	if !s.syncedAt.IsZero() {
		s.drift = differing(s.sessions, fresh) + differing(s.projects, freshProjects)
	}
	s.sessions, s.projects, s.restorable = fresh, freshProjects, restorable
	s.syncedAt = time.Now()
	drift := s.drift
	s.mu.Unlock()

	if drift > 0 {
		// Loud on purpose. Either the broadcast rules missed something or a frame was
		// lost, and both mean the phone was shown something that was not true.
		log.Printf("store drift: %d row(s) differed from the daemon at resync", drift)
	}
	s.publish(change{Kind: "sessions"})
}

// differing counts the rows that changed, appeared or vanished between two snapshots.
func differing(was, now map[string]map[string]any) int {
	n := 0
	for id, a := range was {
		b, ok := now[id]
		if !ok || !sameRow(a, b) {
			n++
		}
	}
	for id := range now {
		if _, ok := was[id]; !ok {
			n++
		}
	}
	return n
}

// sameRow compares two daemon rows by their JSON, which is how they arrived.
func sameRow(a, b map[string]any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// listRows runs one of the daemon's list calls and returns its rows.
func listRows(reqType, key string) ([]map[string]any, error) {
	res, err := client.Call(map[string]any{"type": reqType}, reqType, 15*time.Second)
	if err != nil {
		return nil, err
	}
	return rows(res[key]), nil
}

func rows(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

// ---- telling the phone ----

// subscribe returns a channel of changes and the function that stops it.
func (s *store) subscribe() (<-chan change, func()) {
	ch := make(chan change, 32)
	s.mu.Lock()
	s.subs[ch] = true
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		if s.subs[ch] {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

// publish tells every subscriber. A subscriber that has fallen 32 changes behind is
// dropped rather than blocked: reconnecting re-reads everything, which is the same
// answer it would have caught up to.
func (s *store) publish(c change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- c:
		default:
			delete(s.subs, ch)
			close(ch)
		}
	}
}
