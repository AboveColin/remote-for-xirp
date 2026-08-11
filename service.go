package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// launchd, not a Swift agent. This has to run as the logged-in user — it reads
// the Xirp app's process environment for the daemon token, which `ps -E` only
// permits for the same user — so it is a LaunchAgent in the GUI domain, not a
// LaunchDaemon. A LaunchDaemon runs as root in a different session and would see
// neither the app nor its environment.
const (
	serviceLabel = "dev.xirp.remote"
	defaultAddr  = "0.0.0.0:8790"
)

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
}

func logPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", "xirp-remote.log")
}

func installedBinPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin", "xirp-remote")
}

func guiTarget() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceLabel) }

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// portHolder reports what is listening on the address, so install can refuse to
// fight another supervisor for the port instead of flapping against it.
func portHolder(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-Fcp").Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	var pid, cmd string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "p") {
			pid = line[1:]
		}
		if strings.HasPrefix(line, "c") {
			cmd = line[1:]
		}
	}
	if pid == "" {
		return ""
	}
	return fmt.Sprintf("%s (pid %s)", cmd, pid)
}

func writePlist(bin, addr, key, publicURL string) error {
	env := fmt.Sprintf("\n      <key>XIRP_REMOTE_ADDR</key>\n      <string>%s</string>", addr)
	if key != "" {
		env += fmt.Sprintf("\n      <key>XIRP_REMOTE_KEY</key>\n      <string>%s</string>", key)
	}
	if publicURL != "" {
		env += fmt.Sprintf("\n      <key>XIRP_REMOTE_URL</key>\n      <string>%s</string>", publicURL)
	}
	// PATH matters: transcripts come from `squab`, which is run with `node`. A
	// LaunchAgent inherits a minimal PATH that does not include Homebrew, so node
	// would not be found and every transcript would come back empty.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>serve</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>%s
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, serviceLabel, bin, env, logPath(), logPath())

	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(plistPath(), []byte(plist), 0o644)
}

func cmdInstall(args []string) error {
	// Re-running install is the upgrade path, so it must not lose configuration.
	// Reading the flags over a blank slate meant a plain `xirp-remote install`
	// silently removed the access key and turned authentication off — an upgrade
	// that quietly unlocks the door. Existing values are the defaults; flags and
	// environment override them, and --no-key removes a key on purpose.
	addr, copyBin := defaultAddr, true
	key := firstNonEmpty(os.Getenv("XIRP_REMOTE_KEY"), plistEnv("XIRP_REMOTE_KEY"))
	publicURL := firstNonEmpty(os.Getenv("XIRP_REMOTE_URL"), plistEnv("XIRP_REMOTE_URL"))
	if existing := plistEnv("XIRP_REMOTE_ADDR"); existing != "" {
		addr = existing
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--addr":
			if i+1 < len(args) {
				addr = args[i+1]
				i++
			}
		case "--key":
			if i+1 < len(args) {
				key = args[i+1]
				i++
			}
		case "--url":
			if i+1 < len(args) {
				publicURL = strings.TrimRight(args[i+1], "/")
				i++
			}
		case "--no-copy":
			copyBin = false
		case "--no-key":
			key = ""
		case "--generate-key":
			k, err := generateKey()
			if err != nil {
				return err
			}
			key = k
		}
	}

	if holder := portHolder(addr); holder != "" && !serviceRunning() {
		return fmt.Errorf("%s is already held by %s.\n"+
			"Stop that first, or pass --addr to use a different port, then run install again", addr, holder)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	bin := self
	if copyBin {
		// Copy out of the build directory so a `git clean` or a rebuild cannot
		// leave launchd pointing at a binary that no longer exists.
		bin = installedBinPath()
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return err
		}
		if bin != self {
			data, err := os.ReadFile(self)
			if err != nil {
				return err
			}
			if err := os.WriteFile(bin+".new", data, 0o755); err != nil {
				return err
			}
			if err := os.Rename(bin+".new", bin); err != nil {
				return err
			}
		}
	}

	if err := writePlist(bin, addr, key, publicURL); err != nil {
		return err
	}
	// bootout first so install doubles as upgrade; a failure here just means it
	// was not loaded.
	//
	// bootout is asynchronous. Bootstrapping while the old job is still on its way
	// out fails with "service already loaded", and because that left the job booted
	// out, the result was a service that was installed, enabled, and not running.
	// So wait for it to actually be gone.
	_, _ = launchctl("bootout", guiTarget())
	for i := 0; i < 40; i++ {
		if _, err := launchctl("print", guiTarget()); err != nil {
			break // print fails once the job is unloaded, which is what we want
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out, err := launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath()); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	_, _ = launchctl("enable", guiTarget())

	fmt.Printf("installed %s\n  binary %s\n  plist  %s\n  log    %s\n  addr   %s\n  auth   %s\n",
		serviceLabel, bin, plistPath(), logPath(), addr,
		map[bool]string{true: "key required", false: "OPEN, no authentication"}[key != ""])
	if publicURL != "" {
		fmt.Printf("  url    %s\n", publicURL)
	}

	if err := waitHealthy(addr, 15*time.Second); err != nil {
		return fmt.Errorf("service installed but not answering: %w (see `xirp-remote logs`)", err)
	}
	fmt.Println("service is up and answering /healthz")

	if publicURL != "" {
		os.Setenv("XIRP_REMOTE_URL", publicURL)
	}
	if key != "" {
		remoteKey = key
		fmt.Println("\nScan to pair a phone:")
		if err := cmdQR(); err != nil {
			fmt.Println("(could not render the QR code:", err, ")")
		}
	}
	return nil
}

func cmdUninstall() error {
	out, err := launchctl("bootout", guiTarget())
	if err != nil && !strings.Contains(out, "No such process") {
		fmt.Printf("bootout: %s\n", out)
	}
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("uninstalled %s\n(binary at %s and log at %s were left in place)\n",
		serviceLabel, installedBinPath(), logPath())
	return nil
}

func serviceRunning() bool {
	out, err := launchctl("print", guiTarget())
	return err == nil && strings.Contains(out, "state = running")
}

func servicePID() string {
	out, err := launchctl("print", guiTarget())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "pid =") {
			return strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
		}
	}
	return ""
}

func waitHealthy(addr string, d time.Duration) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	url := "http://127.0.0.1:" + port + "/healthz"
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec // fixed loopback URL
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last
}

// cmdStatus answers the question the user actually has, which is not "is the
// process alive" but "can I use it right now". Those differ: the service can be
// running perfectly while Xirp is quit, in which case there is nothing to control.
func cmdStatus() error {
	installed := true
	if _, err := os.Stat(plistPath()); os.IsNotExist(err) {
		installed = false
	}
	fmt.Printf("service   %s\n", map[bool]string{true: "installed", false: "not installed"}[installed])
	if installed {
		state := "stopped"
		if serviceRunning() {
			state = "running"
			if pid := servicePID(); pid != "" {
				state += " (pid " + pid + ")"
			}
		}
		fmt.Printf("launchd   %s\n", state)
	}

	addr := os.Getenv("XIRP_REMOTE_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	if err := waitHealthy(addr, 2*time.Second); err != nil {
		fmt.Printf("http      not answering on %s (%v)\n", addr, err)
	} else {
		fmt.Printf("http      answering on %s\n", addr)
	}

	if creds, err := discover(); err != nil {
		fmt.Printf("xirp      not reachable: %v\n", err)
	} else {
		fmt.Printf("xirp      daemon on 127.0.0.1:%s, token discovered\n", creds.Port)
		if res, err := client.Call(map[string]any{"type": "sessions:list"}, "sessions:list", 10*time.Second); err == nil {
			list, _ := res["sessions"].([]any)
			fmt.Printf("sessions  %d\n", len(list))
		}
	}
	return nil
}

func cmdLogs(args []string) error {
	tailArgs := []string{"-n", "50"}
	for _, a := range args {
		if a == "-f" || a == "--follow" {
			tailArgs = append(tailArgs, "-f")
		}
	}
	tailArgs = append(tailArgs, logPath())
	cmd := exec.Command("tail", tailArgs...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func cmdRestart() error {
	if out, err := launchctl("kickstart", "-k", guiTarget()); err != nil {
		return fmt.Errorf("kickstart: %v: %s", err, out)
	}
	fmt.Println("restarted")
	return nil
}

func usage() {
	fmt.Print(`xirp-remote — mobile web control for the Xirp agent daemon

USAGE
  xirp-remote [serve]              Run in the foreground (default)
  xirp-remote install [flags]      Install and start as a launchd user agent
  xirp-remote uninstall            Stop and remove the launchd agent
  xirp-remote start | stop | restart
  xirp-remote status               Service, HTTP and Xirp daemon state
  xirp-remote logs [-f]            Show the service log
  xirp-remote qr                   Print a pairing QR code for a phone

INSTALL FLAGS
  --addr <host:port>   Listen address (default 0.0.0.0:8790)
  --key <key>          Require this access key (default: open, no auth)
  --generate-key       Generate a random access key and show its pairing QR
  --no-key             Remove the access key (run open, no authentication)
  --url <base>         Public base URL to encode in pairing QR codes
  --no-copy            Run from the current binary path instead of ~/.local/bin

ENVIRONMENT
  XIRP_REMOTE_ADDR     Listen address for ` + "`serve`" + `
  XIRP_REMOTE_KEY      Access key; unset means open, no authentication
  XIRP_REMOTE_URL      Public base URL to put in the pairing QR (e.g. your proxy)

The agent runs in your GUI login session on purpose: it reads the Xirp app's
process environment for the daemon token, which only works as the same user.
`)
}

func runCLI(args []string) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	if args[0] != "serve" {
		// CLI output is for reading. The daemon client logs its connection on
		// stdout, which belongs in the service log, not interleaved with a status
		// report someone is trying to scan.
		log.SetOutput(io.Discard)
	}
	switch args[0] {
	case "serve":
		return false, nil
	case "install":
		return true, cmdInstall(args[1:])
	case "uninstall":
		return true, cmdUninstall()
	case "start":
		out, e := launchctl("kickstart", guiTarget())
		if e != nil {
			return true, fmt.Errorf("start: %v: %s", e, out)
		}
		fmt.Println("started")
		return true, nil
	case "stop":
		out, e := launchctl("kill", "SIGTERM", guiTarget())
		if e != nil {
			return true, fmt.Errorf("stop: %v: %s", e, out)
		}
		fmt.Println("stopped (KeepAlive will restart it; use `uninstall` to stop for good)")
		return true, nil
	case "restart":
		return true, cmdRestart()
	case "status":
		return true, cmdStatus()
	case "logs":
		return true, cmdLogs(args[1:])
	case "qr", "pair":
		return true, cmdQR()
	case "-h", "--help", "help":
		usage()
		return true, nil
	default:
		usage()
		return true, fmt.Errorf("unknown command %q", args[0])
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
