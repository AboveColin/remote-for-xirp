package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// A file has to land somewhere the agent can read and the repository never sees, and its
// path has to reach the agent's input without being sent.
func TestUploadWritesTheFileAndTypesItsPath(t *testing.T) {
	var mu sync.Mutex
	var typed map[string]any
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {
		if req["type"] == "session:message" {
			mu.Lock()
			typed = req
			mu.Unlock()
		}
	})
	seen := func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return typed
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/s1/upload?name=screenshot.png", strings.NewReader("PNGDATA"))
	sessionUpload(rec, req, "s1")
	if rec.Code != 200 {
		t.Fatalf("status %d (%s)", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	path, _ := out["path"].(string)
	if path == "" {
		t.Fatalf("no path in the answer: %s", rec.Body.String())
	}
	defer os.RemoveAll(uploadDir("s1"))

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != "PNGDATA" {
		t.Fatalf("file holds %q", body)
	}
	if !strings.HasSuffix(path, "-screenshot.png") {
		t.Fatalf("path %q does not end with the name given", path)
	}
	if strings.Contains(path, "/worktree") || !strings.Contains(path, os.TempDir()) {
		t.Fatalf("path %q is not in the temp directory", path)
	}

	waitFor(t, "the path to be typed into the session", func() bool { return seen() != nil })
	frame := seen()
	if got, _ := frame["text"].(string); !strings.Contains(got, path) {
		t.Fatalf("typed %q, want the path", got)
	}
	// A file on its own is not an instruction, so it must not press enter.
	if frame["enter"] != false {
		t.Fatalf("enter = %v, want false", frame["enter"])
	}
}

// The refusal has to name the limit, and nothing may be written when it trips.
func TestUploadRefusalNamesTheLimit(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/s2/upload?name=huge.bin",
		strings.NewReader(strings.Repeat("x", uploadCap+1)))
	sessionUpload(rec, req, "s2")
	if rec.Code != 413 {
		t.Fatalf("status %d, want 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "10 MB") {
		t.Fatalf("refusal %q does not name the limit", rec.Body.String())
	}
	if _, err := os.Stat(uploadDir("s2")); err == nil {
		t.Fatal("a refused upload created its directory anyway")
	}
}

// A name from a phone is not to be trusted with a path.
func TestUploadNamesCannotEscape(t *testing.T) {
	for _, given := range []string{"../../etc/passwd", "/etc/passwd", "..", "", "a/b/c.png", "  spaced name .png"} {
		got := safeName(given)
		if strings.ContainsAny(got, "/\\") || got == "" || got == "." || got == ".." {
			t.Fatalf("safeName(%q) = %q", given, got)
		}
	}
	if got := safeName("shot.png"); got != "shot.png" {
		t.Fatalf("a plain name became %q", got)
	}
}

func TestUploadRefusesAnEmptyBody(t *testing.T) {
	newFakeDaemon(t, func(req map[string]any, send func(map[string]any)) {})
	rec := httptest.NewRecorder()
	sessionUpload(rec, httptest.NewRequest("POST", "/api/sessions/s3/upload", strings.NewReader("")), "s3")
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}
