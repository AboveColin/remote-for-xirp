package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// Embedded files have a zero modification time, so http.FileServer sends neither
// Last-Modified nor ETag. With no validator, Chrome applies heuristic caching and
// happily serves a stale app.js after the binary has been rebuilt — which is how a
// freshly deployed UI kept coming up as the previous version.
//
// So: hash every embedded file once at startup, serve it with a strong ETag and
// `Cache-Control: no-cache`. "no-cache" does not mean "do not store"; it means
// "revalidate before use", which turns the common case into a 304 with no body while
// making a stale asset impossible.
type staticServer struct {
	files map[string]staticFile
}

type staticFile struct {
	data  []byte
	etag  string
	ctype string
}

func newStaticServer(src fs.FS) (*staticServer, error) {
	s := &staticServer{files: map[string]staticFile{}}
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		s.files["/"+p] = staticFile{
			data:  data,
			etag:  `"` + hex.EncodeToString(sum[:8]) + `"`,
			ctype: contentType(p),
		}
		return nil
	})
	return s, err
}

func contentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".json", ".webmanifest":
		return "application/manifest+json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

func (s *staticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/" {
		p = "/index.html"
	}
	f, ok := s.files[p]
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("ETag", f.etag)
	// The service worker must never be served from cache, or a bad worker cannot be
	// replaced. The rest revalidates.
	if p == "/sw.js" {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Content-Type", f.ctype)

	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	http.ServeContent(w, r, p, time.Time{}, bytes.NewReader(f.data))
}
