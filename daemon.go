package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

// discover reads the token out of the running app's process environment. `ps -E`
// only exposes the environment of processes owned by the same user, which is the
// property this relies on: the bridge must run as the user running Xirp.
func discover() (Creds, error) {
	var c Creds

	pgrep := exec.Command("pgrep", "-f", "Xirp")
	out, err := pgrep.Output()
	if err != nil {
		return c, fmt.Errorf("no Xirp process found (is the app running?): %w", err)
	}
	for _, pid := range strings.Fields(string(out)) {
		env, err := exec.Command("ps", "-E", "-o", "command=", "-p", pid).Output()
		if err != nil {
			continue
		}
		if m := tokenRe.FindSubmatch(env); m != nil {
			c.Token = string(m[1])
			if p := portRe.FindSubmatch(env); p != nil {
				c.Port = string(p[1])
			}
			break
		}
	}
	if c.Token == "" {
		return c, fmt.Errorf("found Xirp processes but none exposed CHIRP_WS_TOKEN")
	}
	if c.Port == "" {
		// Fall back to the port file the app maintains.
		home, _ := os.UserHomeDir()
		b, err := os.ReadFile(filepath.Join(home, ".chirp", "daemon-external.port"))
		if err != nil {
			return c, fmt.Errorf("token found but no port (env or ~/.chirp): %w", err)
		}
		c.Port = strings.TrimSpace(string(b))
	}
	return c, nil
}

// Client is a request/response wrapper over the daemon's WebSocket.
//
// The protocol has no request IDs: a reply is matched only by its `type`. So
// requests are serialized under a mutex and each waits for the one response
// type it expects. That caps throughput at one in-flight call, which is far
// above what a phone UI generates and removes any chance of crossing replies.
type Client struct {
	mu    sync.Mutex
	conn  *websocket.Conn
	creds Creds
}

func NewClient() *Client { return &Client{} }

func (c *Client) connect() error {
	if c.conn != nil {
		return nil
	}
	creds, err := discover()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("ws://127.0.0.1:%s/?token=%s", creds.Port, creds.Token)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("dial daemon on :%s: %w", creds.Port, err)
	}
	c.conn = conn
	c.creds = creds
	log.Printf("connected to Xirp daemon on 127.0.0.1:%s", creds.Port)
	return nil
}

func (c *Client) drop() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Call sends one message and returns the first reply whose type is wantType.
// Unrelated broadcasts (the daemon pushes session/terminal events unprompted)
// are skipped. An `error` reply for our request type is surfaced as an error.
func (c *Client) Call(req map[string]any, wantType string, timeout time.Duration) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	res, err := c.call(req, wantType, timeout)
	if err != nil && c.conn == nil {
		// Connection was dropped (app restarted, token rotated). Retry once so a
		// restart of Xirp doesn't require a restart of the bridge.
		if err2 := c.connect(); err2 != nil {
			return nil, err2
		}
		return c.call(req, wantType, timeout)
	}
	return res, err
}

func (c *Client) call(req map[string]any, wantType string, timeout time.Duration) (map[string]any, error) {
	if err := c.connect(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	c.conn.SetWriteDeadline(time.Now().Add(timeout))
	if err := c.conn.WriteMessage(websocket.TextMessage, body); err != nil {
		c.drop()
		return nil, fmt.Errorf("write %s: %w", req["type"], err)
	}
	if wantType == "" {
		return nil, nil
	}

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for %s", wantType)
		}
		c.conn.SetReadDeadline(deadline)
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			c.drop()
			return nil, fmt.Errorf("read while waiting for %s: %w", wantType, err)
		}
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch msg["type"] {
		case wantType:
			return msg, nil
		case "error":
			if orig, ok := msg["originalType"].(string); ok && orig == req["type"] {
				return nil, fmt.Errorf("daemon rejected %s: %v", orig, msg["message"])
			}
		}
	}
}

// Fire sends a message that has no declared response type (`session:message` is
// one: it types into the agent's terminal and the daemon acknowledges nothing).
func (c *Client) Fire(req map[string]any) error {
	_, err := c.Call(req, "", 5*time.Second)
	return err
}

// CallStream collects the daemon's streaming replies.
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
func (c *Client) CallStream(req map[string]any, wantType string, timeout time.Duration) ([]map[string]any, error) {
	const idleGap = 600 * time.Millisecond
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.connect(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	c.conn.SetWriteDeadline(time.Now().Add(timeout))
	if err := c.conn.WriteMessage(websocket.TextMessage, body); err != nil {
		c.drop()
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

	for time.Now().Before(deadline) {
		readBy := time.Now().Add(idleGap)
		if readBy.After(deadline) {
			readBy = deadline
		}
		c.conn.SetReadDeadline(readBy)
		_, raw, err := c.conn.ReadMessage()
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
					c.drop()
					return frames, nil
				}
				continue
			}
			if len(frames) > 0 {
				c.drop()
				return frames, nil
			}
			c.drop()
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
		source, _ := msg["source"].(string)
		seen[source] = true
		if d, _ := msg["done"].(bool); d {
			done[source] = true
		}
	}
	c.drop()
	return frames, nil
}

// ParseSession shells out to squab, the orchestrator CLI shipped inside the app,
// for a transcript. The daemon's own `messages:list` returns rows from its
// database, which is empty for harness-driven sessions; squab's `session-parse`
// is the canonical harness-agnostic reader (schema squab.session-parsed/v1).
func (c *Client) ParseSession(sessionID string, limit int) (map[string]any, error) {
	squab := os.Getenv("CHIRP_SQUAB_PATH")
	if squab == "" {
		squab = "/Applications/Xirp.app/Contents/Resources/app.asar.unpacked/node_modules/@chirp/squab/dist/cli.js"
	}
	if _, err := os.Stat(squab); err != nil {
		return nil, fmt.Errorf("squab CLI not found at %s", squab)
	}
	// No --limit. Its limit is a HEAD, not a tail: `--limit 3` on a 1111-message
	// session returns messages 0, 1 and 2. Passing the user's page size therefore
	// pinned every conversation to its opening exchange and it never appeared to
	// update again, because the first N messages of a growing session never change.
	//
	// The full parse is cheap enough to take instead and slice: measured at 0.07s
	// for that 1111-message session, producing 3.5 MB of JSON.
	cmd := exec.Command("node", squab, "session-parse", sessionID) //nolint:gosec // fixed argv, no shell
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("squab session-parse %s: %w", sessionID, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("squab returned non-JSON for %s: %w", sessionID, err)
	}

	// Keep the newest `limit` messages, which is what a phone is scrolled to.
	if msgs, ok := parsed["messages"].([]any); ok {
		parsed["totalMessages"] = len(msgs)
		if limit > 0 && len(msgs) > limit {
			parsed["messages"] = msgs[len(msgs)-limit:]
			parsed["truncatedFromStart"] = true
		}
	}
	return parsed, nil
}
