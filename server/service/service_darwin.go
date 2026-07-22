//go:build darwin

package service

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// launchdManager registers fw-indexer as a per-user launchd LaunchAgent. A
// LaunchAgent (~/Library/LaunchAgents, loaded into the user's GUI session) needs no
// root and runs as the user — the macOS analogue of `systemd --user`.
//
// ⚠️ COMPILE-CHECKED, RUNTIME-UNVERIFIED. This is pure Go (plist write + launchctl
// exec, no cgo), so it cross-compiles and the logic is reviewable — but launchctl's
// load/bootstrap semantics vary across macOS versions and none of it has been run.
// See ../docs/DAEMON_PLAN.md, "macOS", before trusting it. In particular the
// load-vs-bootstrap choice below is the most likely thing to need adjustment.
type launchdManager struct{}

func newManager() Manager { return launchdManager{} }

// plistPath is ~/Library/LaunchAgents/com.filesworkbench.fw-indexer.plist.
func (launchdManager) plistPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", LabelDarwin+".plist")
}

func (m launchdManager) Describe() string {
	return "launchd LaunchAgent at " + m.plistPath()
}

func (m launchdManager) Install(cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	plist, err := m.renderPlist(cfg)
	if err != nil {
		return err
	}
	path := m.plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(path, plist, 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	// `load -w` loads the agent and clears any "disabled" override so it also starts
	// at future logins. It's the widely-compatible spelling; the modern equivalent is
	// `launchctl bootstrap gui/$(id -u) <plist>` — see DAEMON_PLAN.md if this needs
	// swapping on a newer macOS.
	return m.launchctl("load", "-w", path)
}

func (m launchdManager) Uninstall() error {
	// Best-effort unload (ignore errors — may not be loaded), then remove the plist.
	m.launchctl("unload", "-w", m.plistPath())
	if err := os.Remove(m.plistPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}
	return nil
}

func (m launchdManager) Status() (State, error) {
	if _, err := os.Stat(m.plistPath()); os.IsNotExist(err) {
		return StateNotInstalled, nil
	}
	// `launchctl list <label>` exits 0 when the agent is loaded; the printed record
	// includes a "PID" key only while it's actually running. Absent a PID we report
	// stopped (loaded but not currently running).
	out, err := exec.Command("launchctl", "list", LabelDarwin).CombinedOutput()
	if err != nil {
		return StateStopped, nil // plist present but not loaded
	}
	if strings.Contains(string(out), "\"PID\"") {
		return StateRunning, nil
	}
	return StateStopped, nil
}

func (m launchdManager) launchctl(args ...string) error {
	if out, err := exec.Command("launchctl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// renderPlist builds the LaunchAgent property list. ProcessType=Background asks the
// system to treat it as idle-priority (the resource-discipline lock); KeepAlive with
// SuccessfulExit=false restarts it only on crash, matching Restart=on-failure.
func (m launchdManager) renderPlist(cfg Config) ([]byte, error) {
	// Build a plist by hand (encoding/xml gets Apple's DOCTYPE + <plist> wrapper
	// wrong); keys are ordered for a stable, readable file.
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")

	b.WriteString("  <key>Label</key>\n  <string>" + esc(LabelDarwin) + "</string>\n")

	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	b.WriteString("    <string>" + esc(cfg.Exe) + "</string>\n")
	for _, a := range cfg.Args {
		b.WriteString("    <string>" + esc(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")

	if len(cfg.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, kv := range sortedEnv(cfg.Env) {
			k, v, _ := strings.Cut(kv, "=")
			b.WriteString("    <key>" + esc(k) + "</key>\n    <string>" + esc(v) + "</string>\n")
		}
		b.WriteString("  </dict>\n")
	}

	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <dict>\n")
	b.WriteString("    <key>SuccessfulExit</key>\n    <false/>\n")
	b.WriteString("  </dict>\n")
	b.WriteString("  <key>ProcessType</key>\n  <string>Background</string>\n")

	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String()), nil
}

// esc XML-escapes a plist string value.
func esc(s string) string {
	var buf strings.Builder
	xml.EscapeText(&buf, []byte(s)) //nolint:errcheck // strings.Builder never errors
	return buf.String()
}
