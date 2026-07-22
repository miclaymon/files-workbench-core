//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// systemdManager registers fw-indexer as a `systemd --user` service. A user service
// (not a system one) needs no root: the unit lives under the user's config dir and
// runs as the user. It starts at login; to keep it warm even when the user is logged
// out, enable lingering (`loginctl enable-linger $USER`) — noted in Describe and the
// plan doc, not forced here (it's a separate privilege/policy choice).
//
// RUNTIME-TESTED on the Linux dev machine (install → status → uninstall).
type systemdManager struct{}

func newManager() Manager { return systemdManager{} }

func (systemdManager) unitName() string { return Name + ".service" }

// unitPath is ~/.config/systemd/user/fw-indexer.service (honoring XDG_CONFIG_HOME).
func (m systemdManager) unitPath() string {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(cfg, "systemd", "user", m.unitName())
}

func (m systemdManager) Describe() string {
	return "systemd --user unit at " + m.unitPath() +
		" (for warmth while logged out: `loginctl enable-linger $USER`)"
}

func (m systemdManager) Install(cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	unit := m.renderUnit(cfg)
	path := m.unitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create unit dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := m.systemctl("daemon-reload"); err != nil {
		return err
	}
	// enable --now: start immediately and on future logins.
	return m.systemctl("enable", "--now", m.unitName())
}

func (m systemdManager) Uninstall() error {
	// Best-effort stop/disable (ignore errors — it may already be gone), then remove
	// the unit file and reload so systemd forgets it.
	m.systemctl("disable", "--now", m.unitName())
	if err := os.Remove(m.unitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit: %w", err)
	}
	m.systemctl("daemon-reload")
	return nil
}

func (m systemdManager) Status() (State, error) {
	if _, err := os.Stat(m.unitPath()); os.IsNotExist(err) {
		return StateNotInstalled, nil
	}
	// `is-active` prints "active"/"inactive"/... and exits non-zero when not active,
	// so drive off the printed word rather than the exit code.
	out, _ := exec.Command("systemctl", "--user", "is-active", m.unitName()).Output()
	if strings.TrimSpace(string(out)) == "active" {
		return StateRunning, nil
	}
	return StateStopped, nil
}

func (m systemdManager) systemctl(args ...string) error {
	full := append([]string{"--user"}, args...)
	cmd := exec.Command("systemctl", full...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// renderUnit builds the .service file. Idle I/O + lowest CPU priority keep the
// background walk/scan invisible (the "minimal resource use" lock in INDEX.md);
// Restart=on-failure gives the same crash-resilience core's supervisor provided.
func (m systemdManager) renderUnit(cfg Config) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=" + DisplayName + "\n")
	b.WriteString("After=default.target\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + quoteExec(cfg.Exe, cfg.Args) + "\n")
	for _, kv := range sortedEnv(cfg.Env) {
		// Quote the whole KEY=VALUE so values with spaces survive systemd parsing.
		b.WriteString("Environment=\"" + kv + "\"\n")
	}
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("Nice=19\n")
	b.WriteString("IOSchedulingClass=idle\n\n")

	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// quoteExec renders an ExecStart line, double-quoting the exe and any arg that
// contains whitespace (systemd honors double-quotes in ExecStart).
func quoteExec(exe string, args []string) string {
	parts := []string{maybeQuote(exe)}
	for _, a := range args {
		parts = append(parts, maybeQuote(a))
	}
	return strings.Join(parts, " ")
}

func maybeQuote(s string) string {
	if strings.ContainsAny(s, " \t") {
		return "\"" + s + "\""
	}
	return s
}
