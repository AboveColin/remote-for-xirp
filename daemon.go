package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// The Xirp daemon speaks a typed WebSocket protocol on 127.0.0.1 and
// authenticates with a token passed as a query parameter. Neither the port nor
// the token is written to disk in a stable place: the token is minted per app
// launch and injected into the environment of the app's own processes, so both
// are rediscovered on every connect rather than cached in config.
type Creds struct {
	Port  string
	Token string
}

var (
	tokenRe = regexp.MustCompile(`CHIRP_WS_TOKEN=([0-9a-f]{16,})`)
	portRe  = regexp.MustCompile(`CHIRP_DAEMON_PORT=([0-9]+)`)
)

// discover finds the daemon's port and token.
//
// Both come from the app, and neither is where you would first look:
//
//   - The port comes from ~/.chirp/daemon-external.port, which the app rewrites when
//     it starts. The CHIRP_DAEMON_PORT variable in a process environment cannot be
//     trusted, because an environment is fixed when the process is exec'd: after the
//     app restarts its daemon on a new port, every process started before that still
//     advertises the old one. Observed exactly that — eleven processes claiming :50389
//     while the daemon was on :53546.
//
//   - The token comes from the environment of whichever process is *listening* on that
//     port. Reading the first Xirp process that happens to have a CHIRP_WS_TOKEN picks
//     up a stale token from a session started before the restart, which then fails
//     authentication against the live daemon.
//
// `ps -E` only exposes an environment to the same user, which is the property this
// relies on and the reason this must run as the user running Xirp.
func discover() (Creds, error) {
	var c Creds

	home, _ := os.UserHomeDir()
	if b, err := os.ReadFile(filepath.Join(home, ".chirp", "daemon-external.port")); err == nil {
		c.Port = strings.TrimSpace(string(b))
	}

	// Ask who is listening. That pid's environment holds the matching token.
	if c.Port != "" {
		if out, err := exec.Command("lsof", "-nP", "-iTCP:"+c.Port, "-sTCP:LISTEN", "-t").Output(); err == nil {
			for _, pid := range strings.Fields(string(out)) {
				if tok := tokenOfPID(pid); tok != "" {
					c.Token = tok
					return c, nil
				}
			}
		}
	}

	// Fall back to scanning the app's processes. Prefer one whose advertised port
	// matches the port file, and only then take any token at all.
	pgrep := exec.Command("pgrep", "-f", "Xirp")
	out, err := pgrep.Output()
	if err != nil {
		return c, fmt.Errorf("no Xirp process found (is the app running?): %w", err)
	}
	var anyToken, anyPort string
	for _, pid := range strings.Fields(string(out)) {
		env, err := exec.Command("ps", "-E", "-o", "command=", "-p", pid).Output()
		if err != nil {
			continue
		}
		tok := ""
		if m := tokenRe.FindSubmatch(env); m != nil {
			tok = string(m[1])
		}
		if tok == "" {
			continue
		}
		port := ""
		if p := portRe.FindSubmatch(env); p != nil {
			port = string(p[1])
		}
		if c.Port != "" && port == c.Port {
			c.Token = tok
			return c, nil
		}
		if anyToken == "" {
			anyToken, anyPort = tok, port
		}
	}
	if anyToken == "" {
		return c, fmt.Errorf("found Xirp processes but none exposed CHIRP_WS_TOKEN")
	}
	c.Token = anyToken
	if c.Port == "" {
		c.Port = anyPort
	}
	if c.Port == "" {
		return c, fmt.Errorf("token found but no port (neither ~/.chirp/daemon-external.port nor the environment)")
	}
	return c, nil
}

// discoverCreds is the seam the tests dial through: they point the client at a fake
// daemon instead of at the running app's process environment.
var discoverCreds = discover

// tokenOfPID reads CHIRP_WS_TOKEN out of one process's environment.
func tokenOfPID(pid string) string {
	env, err := exec.Command("ps", "-E", "-o", "command=", "-p", pid).Output()
	if err != nil {
		return ""
	}
	if m := tokenRe.FindSubmatch(env); m != nil {
		return string(m[1])
	}
	return ""
}

// firstDial keeps the startup log to one line. Six sockets dialing would otherwise say
// the same thing six times.
var firstDial sync.Once

// conn is one WebSocket to the daemon, held by one caller at a time.
//
// The protocol has no request ids: a reply is matched by its type, so two calls sharing
// a socket would read each other's answers. One caller per socket makes that impossible,
// and several sockets give several calls at once.
type conn struct {
	ws *websocket.Conn
}

// poolSize is how many calls can be in flight.
//
// One socket under one mutex was the whole client, so every request queued behind every
// other: a slow branch diff blocked the session list for its full 30 seconds, and the
// push watcher blocked it every 20. Six is this app's own peak demand, two per open phone
// for the detail and the pane, plus the push watcher and a resync, and it is the number
// measured to work: the daemon greeted six authenticated clients at once alongside the
// desktop app, and answered each on the socket that asked. Sockets dial on first use, so
// an unused one costs nothing.
const poolSize = 6

// Client is a request/response wrapper over the daemon's WebSocket.
type Client struct {
	free chan *conn
	// calls counts every request sent, which /api/diagnostics reports. It is how you
	// tell whether this app is asking the daemon for things it already knows: an idle
	// phone with the store warm should move it by nothing.
	calls atomic.Int64
}

func NewClient() *Client {
	c := &Client{free: make(chan *conn, poolSize)}
	for i := 0; i < poolSize; i++ {
		c.free <- &conn{}
	}
	return c
}

// take waits for a free socket. The pool is sized to this app's own concurrency, so
// exhaustion means something is stuck rather than busy, and the message says how many.
func (c *Client) take(timeout time.Duration) (*conn, error) {
	select {
	case cn := <-c.free:
		return cn, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("all %d daemon connections were busy for %s", poolSize, timeout)
	}
}

// Close drops every idle socket and keeps the pool whole, so a client stays usable.
func (c *Client) Close() {
	for i := 0; i < poolSize; i++ {
		select {
		case cn := <-c.free:
			cn.drop()
			c.free <- cn
		default:
			return
		}
	}
}

func (cn *conn) connect() error {
	if cn.ws != nil {
		return nil
	}
	// Rediscovered per dial rather than cached: the token is minted per app launch and
	// the port changes with it, so a cached pair is stale exactly when it matters.
	creds, err := discoverCreds()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("ws://127.0.0.1:%s/?token=%s", creds.Port, creds.Token)
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("dial daemon on :%s: %w", creds.Port, err)
	}
	cn.ws = ws
	firstDial.Do(func() { log.Printf("connected to Xirp daemon on 127.0.0.1:%s", creds.Port) })
	return nil
}

func (cn *conn) drop() {
	if cn.ws != nil {
		cn.ws.Close()
		cn.ws = nil
	}
}

// timeoutError says the daemon did not answer in time. It is a distinct type because
// it is the one failure Call must not retry: the request is already on the wire and the
// daemon may still act on it, so a resend of session:create makes a second session.
type timeoutError struct{ want string }

func (e timeoutError) Error() string { return "the daemon did not answer with " + e.want + " in time" }

// Call sends one message and returns the first reply whose type is wantType. It skips
// the unrelated broadcasts the daemon pushes unprompted, and it surfaces errors in both
// shapes they arrive in:
//
//   - a generic `{type:"error", originalType:<our request>}` frame,
//   - one of the daemon's own typed error frames, named per call in errTypes,
//     because the naming is not uniform: `git:error` covers the whole git category
//     while `session:swap-agent:error` belongs to that one request. Their
//     responseTypes in `api:describe` say which a call can receive.
func (c *Client) Call(req map[string]any, wantType string, timeout time.Duration, errTypes ...string) (map[string]any, error) {
	c.calls.Add(1)
	cn, err := c.take(timeout)
	if err != nil {
		return nil, err
	}
	defer func() { c.free <- cn }()

	res, err := cn.call(req, wantType, timeout, errTypes...)
	var te timeoutError
	if err != nil && cn.ws == nil && !errors.As(err, &te) {
		// The connection broke (app restarted, token rotated). Retry once so a
		// restart of Xirp doesn't require a restart of the bridge.
		if err2 := cn.connect(); err2 != nil {
			return nil, err2
		}
		return cn.call(req, wantType, timeout, errTypes...)
	}
	return res, err
}

func (cn *conn) call(req map[string]any, wantType string, timeout time.Duration, errTypes ...string) (map[string]any, error) {
	if err := cn.connect(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	cn.ws.SetWriteDeadline(time.Now().Add(timeout))
	if err := cn.ws.WriteMessage(websocket.TextMessage, body); err != nil {
		cn.drop()
		return nil, fmt.Errorf("write %s: %w", req["type"], err)
	}
	if wantType == "" {
		return nil, nil
	}

	deadline := time.Now().Add(timeout)
	for {
		// Out of time. Drop the connection rather than keep it: the answer may still
		// be on its way, and the next call waiting for that same type would read it as
		// its own. One redial costs a discover plus a dial.
		if time.Now().After(deadline) {
			cn.drop()
			return nil, timeoutError{want: wantType}
		}
		cn.ws.SetReadDeadline(deadline)
		_, raw, err := cn.ws.ReadMessage()
		if err != nil {
			cn.drop()
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil, timeoutError{want: wantType}
			}
			return nil, fmt.Errorf("read while waiting for %s: %w", wantType, err)
		}
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		mt, _ := msg["type"].(string)
		if mt == wantType && aboutOurSession(req, msg, mt) {
			return msg, nil
		}
		if mt == "error" {
			if orig, ok := msg["originalType"].(string); ok && orig == req["type"] {
				return nil, fmt.Errorf("daemon rejected %s: %v", orig, msg["message"])
			}
			continue
		}
		for _, et := range errTypes {
			if mt == et {
				return nil, daemonError(mt, msg)
			}
		}
	}
}

// sessionOf reads the session a frame is about. The daemon puts it at the top level on
// some types and inside the session row on others.
func sessionOf(msg map[string]any) string {
	if id, _ := msg["sessionId"].(string); id != "" {
		return id
	}
	if sm, ok := msg["session"].(map[string]any); ok {
		id, _ := sm["id"].(string)
		return id
	}
	return ""
}

// aboutOurSession says whether a frame of the wanted type answers this request rather
// than someone else's.
//
// Matching on the type alone is not enough, because the daemon broadcasts several reply
// types for every session: session:updated from 43 places, session:urls, session:created.
// So a status change on any other session could satisfy a wait. Stopping session A then
// reported session B's status, and renaming A returned B's row for the header.
//
// session:created is the one reply that is legitimately about a different session: a fork
// asks about the source and is answered with the copy.
func aboutOurSession(req, msg map[string]any, replyType string) bool {
	if replyType == "session:created" {
		return true
	}
	asked, _ := req["sessionId"].(string)
	if asked == "" {
		return true
	}
	got := sessionOf(msg)
	return got == "" || got == asked
}

// daemonError turns one of the daemon's typed error frames into a Go error.
//
// These carry more than a sentence. `git:error` names a code (DIRECTORY_MISSING when a
// worktree was deleted) and `session:swap-agent:error` adds a hint written for the
// person reading it, such as "restart the session to enable it" for a session created
// before the running build. Dropping those loses the only actionable text there is.
func daemonError(kind string, msg map[string]any) error {
	code, _ := msg["code"].(string)
	text, _ := msg["message"].(string)
	hint, _ := msg["hint"].(string)

	said := code
	if text != "" {
		if said != "" {
			said += ": "
		}
		said += text
	}
	if said == "" {
		return fmt.Errorf("the daemon answered %s with no detail", kind)
	}
	if hint != "" {
		said += " (hint: " + hint + ")"
	}
	return errors.New(said)
}

// Fire sends a message that has no declared response type (`session:message` is
// one: it types into the agent's terminal and the daemon acknowledges nothing).
func (c *Client) Fire(req map[string]any) error {
	_, err := c.Call(req, "", 5*time.Second)
	return err
}

// CallStream borrows a socket and collects a streaming answer on it.
func (c *Client) CallStream(req map[string]any, wantType string, timeout time.Duration) ([]map[string]any, error) {
	c.calls.Add(1)
	cn, err := c.take(timeout)
	if err != nil {
		return nil, err
	}
	defer func() { c.free <- cn }()
	return cn.callStream(req, wantType, timeout)
}

// callStream collects the daemon's streaming replies.
//
// Session search is the reason this is subtle. It answers from three independent
// sources and each one signals its own completion: `metadata` and `messages`
// return in about 4ms already marked done=true, then `jsonl` streams a frame per
// matching transcript for several seconds. So "stop at the first done=true" ends
// the search before the source that finds anything has started, and "wait for the
// deadline" makes every search take the full timeout.
//
// The rule used instead: keep reading until every source seen so far has reported
// done, and no further frame arrives within idleGap. That needs no list of source
// names, so a new source added upstream does not silently truncate results.
//
// Reaching the overall deadline returns what arrived rather than an error —
// partial results are useful, an error page is not.
func (cn *conn) callStream(req map[string]any, wantType string, timeout time.Duration) ([]map[string]any, error) {
	const idleGap = 600 * time.Millisecond
	if err := cn.connect(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	cn.ws.SetWriteDeadline(time.Now().Add(timeout))
	if err := cn.ws.WriteMessage(websocket.TextMessage, body); err != nil {
		cn.drop()
		return nil, fmt.Errorf("write %s: %w", req["type"], err)
	}

	var frames []map[string]any
	seen := map[string]bool{}
	done := map[string]bool{}
	deadline := time.Now().Add(timeout)

	allDone := func() bool {
		if len(seen) == 0 {
			return false
		}
		for s := range seen {
			if !done[s] {
				return false
			}
		}
		return true
	}

	// Idle is measured from the last frame we care about, not the last frame that
	// arrived. The daemon broadcasts unrelated traffic to connected clients — its
	// database debug feed emits a frame per query — and treating that as activity would
	// keep this loop alive until the overall deadline on every call.
	lastWanted := time.Now()
	for time.Now().Before(deadline) {
		readBy := lastWanted.Add(idleGap)
		if readBy.Before(time.Now()) {
			readBy = time.Now().Add(50 * time.Millisecond)
		}
		if readBy.After(deadline) {
			readBy = deadline
		}
		cn.ws.SetReadDeadline(readBy)
		_, raw, err := cn.ws.ReadMessage()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Quiet on the wire. Finished if every source that spoke is done;
				// otherwise keep waiting for the slow one until the deadline.
				if allDone() {
					// A read deadline can fire part-way through a frame, after which
					// the framing state is no longer trustworthy. Rather than reuse a
					// possibly desynchronised connection, drop it and let the next
					// call redial — that costs one dial and removes a class of bug
					// that would show up later as garbled replies.
					cn.drop()
					return frames, nil
				}
				continue
			}
			if len(frames) > 0 {
				cn.drop()
				return frames, nil
			}
			cn.drop()
			return nil, fmt.Errorf("read while waiting for %s: %w", wantType, err)
		}
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg["type"] != wantType {
			continue
		}
		frames = append(frames, msg)
		lastWanted = time.Now()
		source, _ := msg["source"].(string)
		seen[source] = true
		if d, _ := msg["done"].(bool); d {
			done[source] = true
		}
	}
	cn.drop()
	return frames, nil
}
