package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Knowing an agent finished without looking is the whole reason to have this on a
// phone, and it is the one feature a polling web app cannot provide by itself: a page
// that is closed cannot poll. So the server watches, and the push goes to the browser's
// service worker.
//
// Encryption and VAPID signing are delegated to a library rather than hand-rolled.
// RFC 8291 is ECDH plus HKDF plus AES-GCM with exact salt and info strings; getting one
// of those subtly wrong produces messages that silently never arrive.

const (
	// pushSweep is the safety net, not the trigger. The store tells the watcher when a
	// session moved, so this only catches a change that arrived while nothing was
	// subscribed.
	pushSweep = time.Minute
	// A subscription that keeps failing is dead — the browser was uninstalled, or the
	// endpoint was rotated. Dropped after this many consecutive failures rather than
	// retried forever.
	pushMaxFailures = 5
)

type subscription struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
	Label    string `json:"label,omitempty"`
	Added    string `json:"added"`
	Failures int    `json:"failures,omitempty"`
}

type pushState struct {
	PublicKey  string         `json:"publicKey"`
	PrivateKey string         `json:"privateKey"`
	Subs       []subscription `json:"subscriptions"`
}

var (
	pushMu   sync.Mutex
	pushData pushState
	// Last status seen per session, so a notification fires on a change rather than on
	// every poll. Empty until the first pass, which is what keeps that pass quiet:
	// otherwise starting the service would announce every session that had already
	// finished.
	lastStatus  = map[string]string{}
	pushStarted bool
)

func pushFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "xirp-remote", "push.json")
}

func loadPush() error {
	pushMu.Lock()
	defer pushMu.Unlock()
	b, err := os.ReadFile(pushFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(b, &pushData)
}

// savePush writes atomically: a truncated file here would lose the keypair, and losing
// the keypair invalidates every existing subscription.
func savePushLocked() error {
	if err := os.MkdirAll(filepath.Dir(pushFile()), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(pushData, "", "  ")
	if err != nil {
		return err
	}
	tmp := pushFile() + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, pushFile())
}

// ensureKeys mints a VAPID keypair on first use. The public half identifies this server
// to the browser; the private half signs each request. They must persist, because a new
// keypair means every subscription made against the old one stops working.
func ensureKeys() error {
	pushMu.Lock()
	defer pushMu.Unlock()
	if pushData.PublicKey != "" && pushData.PrivateKey != "" {
		return nil
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return err
	}
	pushData.PrivateKey, pushData.PublicKey = priv, pub
	log.Print("generated a VAPID keypair for push notifications")
	return savePushLocked()
}

func handlePushKey(w http.ResponseWriter, r *http.Request) {
	if err := ensureKeys(); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	pushMu.Lock()
	defer pushMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"publicKey":     pushData.PublicKey,
		"subscriptions": len(pushData.Subs),
	})
}

func handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	var body struct {
		Endpoint string            `json:"endpoint"`
		Keys     map[string]string `json:"keys"`
		Label    string            `json:"label"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad body"})
		return
	}
	if body.Endpoint == "" || body.Keys["p256dh"] == "" || body.Keys["auth"] == "" {
		writeJSON(w, 400, map[string]any{"error": "endpoint, p256dh and auth are required"})
		return
	}

	pushMu.Lock()
	defer pushMu.Unlock()
	for i, s := range pushData.Subs {
		if s.Endpoint == body.Endpoint {
			// Re-subscribing is normal: the browser hands back the same endpoint, and
			// its failure count should reset.
			pushData.Subs[i].P256dh = body.Keys["p256dh"]
			pushData.Subs[i].Auth = body.Keys["auth"]
			pushData.Subs[i].Failures = 0
			if body.Label != "" {
				pushData.Subs[i].Label = body.Label
			}
			if err := savePushLocked(); err != nil {
				writeJSON(w, 500, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true, "updated": true, "subscriptions": len(pushData.Subs)})
			return
		}
	}
	pushData.Subs = append(pushData.Subs, subscription{
		Endpoint: body.Endpoint,
		P256dh:   body.Keys["p256dh"],
		Auth:     body.Keys["auth"],
		Label:    body.Label,
		Added:    time.Now().Format(time.RFC3339),
	})
	if err := savePushLocked(); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("push subscription added (%d total)", len(pushData.Subs))
	writeJSON(w, 200, map[string]any{"ok": true, "subscriptions": len(pushData.Subs)})
}

func handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body)
	pushMu.Lock()
	defer pushMu.Unlock()
	kept := pushData.Subs[:0]
	for _, s := range pushData.Subs {
		if body.Endpoint == "" || s.Endpoint != body.Endpoint {
			kept = append(kept, s)
		}
	}
	pushData.Subs = kept
	if err := savePushLocked(); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "subscriptions": len(pushData.Subs)})
}

// handlePushTest sends a notification now, so the whole chain can be proven without
// waiting for an agent to finish.
func handlePushTest(w http.ResponseWriter, r *http.Request) {
	sent, errs := sendPush(map[string]any{
		"title": "Remote For Xirp",
		"body":  "Notifications are working.",
		"tag":   "test",
	})
	writeJSON(w, 200, map[string]any{"sent": sent, "errors": errs})
}

// sendPush delivers to every subscription and reports what happened. Failures are
// counted, and a subscription that fails repeatedly is dropped: 404 and 410 mean the
// browser is gone for good.
func sendPush(payload map[string]any) (int, []string) {
	if err := ensureKeys(); err != nil {
		return 0, []string{err.Error()}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, []string{err.Error()}
	}

	pushMu.Lock()
	subs := make([]subscription, len(pushData.Subs))
	copy(subs, pushData.Subs)
	priv, pub := pushData.PrivateKey, pushData.PublicKey
	pushMu.Unlock()

	sent := 0
	var errs []string
	gone := map[string]bool{}
	failed := map[string]bool{}

	for _, s := range subs {
		sub := &webpush.Subscription{
			Endpoint: s.Endpoint,
			Keys:     webpush.Keys{P256dh: s.P256dh, Auth: s.Auth},
		}
		res, err := webpush.SendNotification(body, sub, &webpush.Options{
			Subscriber:      "xirp-remote@localhost",
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             600,
			Urgency:         webpush.UrgencyNormal,
		})
		if err != nil {
			errs = append(errs, shortEndpoint(s.Endpoint)+": "+err.Error())
			failed[s.Endpoint] = true
			continue
		}
		res.Body.Close()
		switch {
		case res.StatusCode == 404 || res.StatusCode == 410:
			gone[s.Endpoint] = true
			errs = append(errs, shortEndpoint(s.Endpoint)+": subscription expired, removed")
		case res.StatusCode >= 300:
			failed[s.Endpoint] = true
			errs = append(errs, fmt.Sprintf("%s: HTTP %d", shortEndpoint(s.Endpoint), res.StatusCode))
		default:
			sent++
		}
	}

	pushMu.Lock()
	kept := pushData.Subs[:0]
	for _, s := range pushData.Subs {
		if gone[s.Endpoint] {
			continue
		}
		if failed[s.Endpoint] {
			s.Failures++
			if s.Failures >= pushMaxFailures {
				continue
			}
		} else {
			s.Failures = 0
		}
		kept = append(kept, s)
	}
	pushData.Subs = kept
	_ = savePushLocked()
	pushMu.Unlock()

	return sent, errs
}

func shortEndpoint(e string) string {
	if i := strings.Index(e, "://"); i >= 0 {
		e = e[i+3:]
	}
	if i := strings.Index(e, "/"); i > 0 {
		return e[:i]
	}
	if len(e) > 32 {
		return e[:32]
	}
	return e
}

// startPushWatcher notices sessions finishing and asking for input.
//
// It watches statuses rather than transcripts: `finished` is the turn ending with nobody
// at the desk, `completed` and `failed` end the session, and `waiting` means the agent
// wants something from you.
//
// It reads the store rather than the daemon, so this costs nothing to run and fires when
// the daemon says a session moved instead of up to 20 seconds later.
func startPushWatcher() {
	if pushStarted {
		return
	}
	pushStarted = true
	changes, _ := live.subscribe()
	go func() {
		sweep := time.Tick(pushSweep)
		for {
			select {
			case _, open := <-changes:
				if !open {
					// Dropped for falling behind, which cannot happen at this rate, but
					// a watcher that stops watching would silence notifications.
					changes, _ = live.subscribe()
					continue
				}
			case <-sweep:
			}
			pushMu.Lock()
			subs := len(pushData.Subs)
			pushMu.Unlock()
			if subs == 0 {
				// Nothing subscribed. Keep the statuses current anyway, so switching
				// notifications on does not announce everything that already finished.
				lastStatus = statuses(live.sessionRows())
				continue
			}
			checkForFinished()
		}
	}()
}

// statuses is what to remember when there is nobody to tell.
func statuses(rows []map[string]any) map[string]string {
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		if id != "" {
			out[id], _ = row["status"].(string)
		}
	}
	return out
}

// pushEvent is one buzz: the line it shows, and the session a tap opens.
type pushEvent struct {
	title, body, id string
}

// statusEvents decides which status changes deserve a notification and returns the
// statuses to remember for the next pass.
//
// Kept separate from the sending, and pure, so the rules can be read and tested on
// their own. `prev` decides everything: an empty one yields no events, which is what
// makes the first pass after startup quiet rather than announcing every session that
// finished hours ago.
func statusEvents(raw []map[string]any, names map[string]string, prev map[string]string) ([]pushEvent, map[string]string) {
	now := make(map[string]string, len(raw))
	var events []pushEvent

	for _, sm := range raw {
		id, _ := sm["id"].(string)
		if id == "" {
			continue
		}
		status, _ := sm["status"].(string)
		now[id] = status
		was, known := prev[id]
		if !known || was == status {
			continue
		}

		name, _ := sm["name"].(string)
		if name == "" {
			name = shortID(id)
		}
		projectID, _ := sm["projectId"].(string)
		where := names[projectID]

		switch status {
		case "finished":
			// The agent's turn ended with nobody at the desk. The daemon reserves
			// `idle` for the same event while someone is watching it there, so
			// `finished` is the case this app exists for: it is the status a turn
			// lands in when you are away, and it is not `completed`.
			events = append(events, pushEvent{name, place("Finished", where), id})
		case "completed":
			// The session itself ended: the agent exited, or someone stopped it.
			events = append(events, pushEvent{name, place("Ended", where), id})
		case "failed":
			events = append(events, pushEvent{name, place("Failed", where), id})
		case "waiting":
			reason, _ := sm["waitingReason"].(string)
			if reason == "" {
				reason = "waiting for you"
			}
			events = append(events, pushEvent{name, reason, id})
		}
	}

	sort.Slice(events, func(i, j int) bool { return events[i].title < events[j].title })
	return events, now
}

// place appends the project when there is one, so an unknown project reads as
// "Finished" rather than "Finished in ".
func place(what, where string) string {
	if where == "" {
		return what
	}
	return what + " in " + where
}

// shortID is the fallback label for a session the daemon never named. Sliced rather
// than assumed 36 characters, because a slice past the end panics.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func checkForFinished() {
	events, now := statusEvents(live.sessionRows(), projectNames(), lastStatus)
	// Replacing the map rather than updating it also drops any session the daemon no
	// longer lists, so the table cannot grow for the life of the process.
	lastStatus = now

	for _, e := range events {
		sent, errs := sendPush(map[string]any{
			"title":     e.title,
			"body":      e.body,
			"sessionId": e.id,
			"tag":       "session-" + e.id,
		})
		log.Printf("push: %q -> %d sent, %d errors", e.body, sent, len(errs))
		for _, msg := range errs {
			log.Printf("push error: %s", msg)
		}
	}
}
