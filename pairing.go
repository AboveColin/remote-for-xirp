package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// Pairing: a phone should join by pointing its camera at a code, not by typing 32
// hex characters with a thumb.
//
// The pairing URL carries the key in the fragment (`#k=...`) rather than the query
// string. A fragment is never sent to the server and does not appear in access logs
// or in a proxy's history, and the page strips it from the address bar as soon as it
// has exchanged it for a cookie.

func generateKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// lanAddress turns the listen address into something a phone can open. The listen
// address is usually 0.0.0.0, which is useless in a URL, so the advertised host comes
// from the chosen bind address or the default route.
func lanAddress(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = "", "8790"
	}
	return net.JoinHostPort(advertiseAddress(host), port)
}

// pairingURL prefers an explicit public URL, since a reverse proxy hostname is what
// actually works from outside the LAN.
func pairingURL(key string) string {
	// The CLI runs as a separate process from the service, so it does not inherit
	// the service's environment; the installed plist is the shared record.
	base := strings.TrimRight(firstNonEmpty(os.Getenv("XIRP_REMOTE_URL"), plistEnv("XIRP_REMOTE_URL")), "/")
	if base == "" {
		addr := os.Getenv("XIRP_REMOTE_ADDR")
		if addr == "" {
			addr = defaultAddr
		}
		base = "http://" + lanAddress(addr)
	}
	if key == "" {
		return base + "/"
	}
	return base + "/#k=" + key
}

// cmdQR prints the pairing code in the terminal. `qrcode.New(...).ToSmallString`
// uses half-block characters, which keeps a 33x33 code inside a normal window.
func cmdQR() error {
	if remoteKey == "" {
		remoteKey = os.Getenv("XIRP_REMOTE_KEY")
	}
	if remoteKey == "" {
		// Read it back from the installed service, so `qr` works in a fresh shell.
		if k := keyFromPlist(); k != "" {
			remoteKey = k
		}
	}
	url := pairingURL(remoteKey)
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Println(q.ToSmallString(false))
	fmt.Println(url)
	if remoteKey == "" {
		fmt.Println("\nNo access key is set, so this link only carries the address.")
		fmt.Println("Run `xirp-remote install --generate-key` to require one.")
	}
	return nil
}

// plistEnv reads one EnvironmentVariables entry out of the installed LaunchAgent.
// The plist is where `install` records configuration, so it is also where `install`
// and `qr` read it back from — otherwise re-running either in a fresh shell would
// see no key and conclude there isn't one.
func plistEnv(name string) string {
	data, err := os.ReadFile(plistPath())
	if err != nil {
		return ""
	}
	s := string(data)
	i := strings.Index(s, "<key>"+name+"</key>")
	if i < 0 {
		return ""
	}
	rest := s[i:]
	start := strings.Index(rest, "<string>")
	end := strings.Index(rest, "</string>")
	if start < 0 || end < start {
		return ""
	}
	return rest[start+len("<string>") : end]
}

func keyFromPlist() string { return plistEnv("XIRP_REMOTE_KEY") }

// handlePair serves the pairing code to an already-authenticated browser, so a
// second device can be added from the first without going back to a terminal.
func handlePair(w http.ResponseWriter, r *http.Request) {
	url := pairingURL(remoteKey)
	if r.URL.Query().Get("format") == "png" {
		png, err := qrcode.Encode(url, qrcode.Medium, 512)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
		return
	}
	writeJSON(w, 200, map[string]any{
		"url":       url,
		"hasKey":    remoteKey != "",
		"publicURL": os.Getenv("XIRP_REMOTE_URL"),
	})
}
