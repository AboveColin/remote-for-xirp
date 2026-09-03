package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// The phone polled every five seconds because the daemon's changes stopped at the
// bridge. The store fixed that, so the bridge can say what moved and the phone can stop
// asking.
//
// An event names what changed and carries none of it. The phone re-reads the endpoint it
// already knows, which now costs the daemon nothing, so there is one event shape to write
// and one to read, and a frame lost on the way cannot leave the phone holding half a row.
//
// The poll stays as the fallback. Phones sleep, proxies close idle connections, and a
// stream that quietly stopped is worse than one that was never there.

// streamBeat keeps the connection warm. A proxy or a phone radio will drop a connection
// that says nothing for a minute, and a comment line costs two bytes.
const streamBeat = 25 * time.Second

// maxStreams is a tripwire, not a budget: one Mac is watched by a phone or two, and each
// stream is a goroutine and a 32-entry channel. A refusal names the number, because a
// client that cannot connect and is not told why has nothing to act on.
const maxStreams = 8

var openStreams atomic.Int64

// handleEvents streams what the store changed, as server-sent events.
func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]any{"error": "this server cannot stream"})
		return
	}
	if n := openStreams.Add(1); n > maxStreams {
		openStreams.Add(-1)
		writeJSON(w, 503, map[string]any{
			"error": fmt.Sprintf("%d event streams are already open, which is the limit", maxStreams),
		})
		return
	}
	defer openStreams.Add(-1)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// A reverse proxy buffers an event stream unless it is told not to, and a buffered
	// stream looks exactly like a broken one: the phone connects, nothing arrives.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	changes, stop := live.subscribe()
	defer stop()

	// Reconnect after three seconds rather than the browser's default, and flush at once
	// so the phone knows the stream is live and can lengthen its poll.
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	beat := time.NewTicker(streamBeat)
	defer beat.Stop()

	for {
		select {
		case c, open := <-changes:
			if !open {
				// Dropped for falling behind. Ending the response makes the phone
				// reconnect, and its reconnect re-reads everything, which is the state
				// it would have caught up to.
				return
			}
			frame, err := json.Marshal(c)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", frame)
			flusher.Flush()
		case <-beat.C:
			fmt.Fprint(w, ": beat\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
