package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Sending a picture is the thing a phone can do that a desk cannot: a screenshot of the
// bug, a photo of a whiteboard, a log someone sent you. The daemon has no upload of any
// kind, so the bridge writes the file and types its path into the agent's input.
//
// It writes to the system temp directory rather than into the session's worktree. A file
// dropped in the repository would show up in the agent's own `git status`, in the diff
// screen, and in whatever it commits, and answering that with a .gitignore entry means
// editing someone's repository to add a feature to a phone app. Temp costs nothing and
// the agent can read it: it runs as the same user.
//
// The path is typed without pressing enter, so the message is still yours to write. That
// also means a file arriving alone cannot start a turn by accident.

// uploadCap is a tripwire. A phone screenshot is about 2 MB and a photo about 5, so 10
// passes anything worth sending to an agent and refuses a video with the number in the
// refusal. The whole body is held in memory while it is written, which is the other
// reason not to make this generous.
const uploadCap = 10 << 20

// safeName keeps a filename to something that cannot escape the directory it is written
// to, or confuse a shell that later reads the path.
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = unsafeName.ReplaceAllString(name, "-")
	name = strings.Trim(name, ".-")
	if name == "" {
		name = "upload"
	}
	if len(name) > 80 {
		name = name[len(name)-80:]
	}
	return name
}

// uploadDir is where a session's files go. One directory per session, so it is obvious
// what to delete and nothing collides.
func uploadDir(sessionID string) string {
	return filepath.Join(os.TempDir(), "xirp-remote", safeName(sessionID))
}

// sessionUpload writes one file and types its path into the session's input.
func sessionUpload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "POST only"})
		return
	}
	name := safeName(r.URL.Query().Get("name"))

	// One more byte than the cap allows is read on purpose, so going over is detected
	// rather than silently truncated into a corrupt file.
	body, err := io.ReadAll(io.LimitReader(r.Body, uploadCap+1))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if len(body) == 0 {
		writeJSON(w, 400, map[string]any{"error": "nothing to write"})
		return
	}
	if len(body) > uploadCap {
		writeJSON(w, 413, map[string]any{
			"error": fmt.Sprintf("that file is over the %d MB this sends", uploadCap>>20),
		})
		return
	}

	dir := uploadDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	// The timestamp keeps two files of the same name apart and puts them in order for
	// whoever cleans the directory out.
	path := filepath.Join(dir, time.Now().UTC().Format("20060102-150405")+"-"+name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}

	// enter false: the path lands in the agent's input and waits for you to say what to
	// do with it. A file on its own is not an instruction.
	if err := client.Fire(map[string]any{
		"type": "session:message", "sessionId": id, "text": path + " ", "enter": false,
	}); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "path": path})
		return
	}
	log.Printf("wrote %d bytes to %s for session %s", len(body), path, id)
	writeJSON(w, 200, map[string]any{"ok": true, "path": path, "bytes": len(body)})
}
