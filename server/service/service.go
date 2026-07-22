// Package service installs, removes, and reports on fw-indexer as an OS-managed
// background daemon (the search index's Phase 4 — see ../docs/INDEX.md and
// ../docs/DAEMON_PLAN.md).
//
// Phases 1–3 run fw-indexer as an *app-child*: the core data server spawns it and
// kills it on quit (server/indexer_proxy.go), so the index lives and dies with the
// app. This package is the alternative lifecycle: register fw-indexer with the OS
// service manager (systemd --user on Linux, a launchd LaunchAgent on macOS, the
// Windows Service Control Manager on Windows) so the index stays warm across app
// restarts and even while the app is closed, and is shared by every app instance.
//
// The two models coexist: core's connect-or-spawn logic adopts an already-running
// daemon if one answers on FW_INDEX_ADDR, and otherwise falls back to spawning its
// own child. Installing the daemon is opt-in (a `fw-indexer install` command); a
// stock checkout keeps working with zero service setup.
//
// ── Verification status ───────────────────────────────────────────────────────
// Unlike the Phase-3 native backends, none of this needs cgo — systemd/launchd are
// file writes plus systemctl/launchctl subprocess calls, and the Windows path uses
// the pure-Go golang.org/x/sys/windows/svc + .../mgr. So all three OSes
// cross-compile-check, and the **Linux systemd path is runtime-tested** on the dev
// machine (install → status → uninstall). The macOS launchd and Windows SCM paths
// are written and compile-checked but their *runtime* behavior
// (launchctl/SCM semantics, elevation) is UNVERIFIED — validate on those OSes. See
// ../docs/DAEMON_PLAN.md for the per-OS checklist.
package service

import (
	"fmt"
	"sort"
)

// Identifiers the OS managers key on. Kept here so all three backends agree.
const (
	// Name is the base service identifier: the systemd unit name (minus .service),
	// the Windows service name, and the leaf used in messages.
	Name = "fw-indexer"

	// LabelDarwin is the launchd label (reverse-DNS, also the plist basename).
	LabelDarwin = "com.filesworkbench.fw-indexer"

	// DisplayName is the human-facing description (Windows display name, systemd
	// Description=, launchd is label-only).
	DisplayName = "Files Workbench Search Index"
)

// State is a coarse daemon lifecycle state, uniform across OSes.
type State int

const (
	StateUnknown      State = iota
	StateNotInstalled       // no unit/plist/service registered
	StateStopped            // registered but not running
	StateRunning            // registered and running
)

func (s State) String() string {
	switch s {
	case StateNotInstalled:
		return "not installed"
	case StateStopped:
		return "installed (stopped)"
	case StateRunning:
		return "running"
	default:
		return "unknown"
	}
}

// Config is what to bake into the service definition at install time. The daemon
// gets its entire configuration from here (not from core, since core may not even
// be running when the OS starts it), so Env must carry everything fw-indexer needs:
// FW_INDEX_ROOTS, FW_DATA_DIR, FW_INDEX_ADDR, and any optional FW_INDEX_* tuning.
type Config struct {
	Exe  string            // absolute path to the fw-indexer binary to register
	Args []string          // extra args (usually none — default invocation runs the indexer)
	Env  map[string]string // environment baked into the service definition
}

// Manager installs/removes/queries the daemon for the current OS. Obtain one with
// NewManager(); the concrete type is selected by build tag.
type Manager interface {
	// Install registers (and starts) the daemon, writing the OS service definition.
	// Idempotent-ish: re-installing overwrites the previous definition.
	Install(cfg Config) error
	// Uninstall stops and deregisters the daemon, removing its service definition.
	// A not-installed daemon is not an error.
	Uninstall() error
	// Status reports the current lifecycle state.
	Status() (State, error)
	// Describe returns a short human string naming where the service lives (unit
	// path / plist path / "Windows Service Control Manager") for CLI output.
	Describe() string
}

// NewManager returns the OS-appropriate Manager (see newManager in the per-OS file).
func NewManager() Manager { return newManager() }

// validate checks a Config has the minimum an installer needs.
func (c Config) validate() error {
	if c.Exe == "" {
		return fmt.Errorf("service: Config.Exe (binary path) is required")
	}
	if c.Env["FW_INDEX_ROOTS"] == "" {
		return fmt.Errorf("service: FW_INDEX_ROOTS must be set (explicit-roots policy — the daemon indexes nothing without it)")
	}
	return nil
}

// sortedEnv flattens an env map to deterministic "KEY=VALUE" lines. Stable output
// keeps every OS's generated service definition diff-friendly and reproducible;
// shared by the Linux and macOS renderers.
func sortedEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
