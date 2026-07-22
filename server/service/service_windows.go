//go:build windows

package service

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// scmManager registers fw-indexer with the Windows Service Control Manager. Unlike
// the Linux/macOS per-user agents, a Windows service is system-wide and its default
// account is **LocalSystem** — which is elevated. That's deliberate: the Phase-3
// USN/MFT native backend needs a raw (elevated) volume handle, and a LocalSystem
// service is the clean way to grant that persistently. Installing therefore requires
// an elevated (admin) process; SCM calls fail with access-denied otherwise. To run
// unelevated (portable backend only), set ServiceStartName to the user account and
// FW_INDEX_NATIVE=0 — see ../docs/DAEMON_PLAN.md, "Windows".
//
// ⚠️ COMPILE-CHECKED, RUNTIME-UNVERIFIED. Pure Go (x/sys/windows/svc + mgr +
// registry), so it cross-compiles from Linux, but no part of the SCM interaction —
// create/start/stop/delete, the registry Environment write, the service-side
// svc.Run handler (cmd/fw-indexer/run_windows.go) — has been run on Windows.
type scmManager struct{}

func newManager() Manager { return scmManager{} }

func (scmManager) Describe() string {
	return `Windows Service Control Manager (service "` + Name + `", account LocalSystem)`
}

func (m scmManager) Install(cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	conn, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM (run elevated?): %w", err)
	}
	defer conn.Disconnect()

	// Make install idempotent: tear down a prior registration first.
	if s, err := conn.OpenService(Name); err == nil {
		s.Control(svc.Stop) //nolint:errcheck // best-effort; may not be running
		s.Delete()          //nolint:errcheck
		s.Close()
	}

	s, err := conn.CreateService(Name, cfg.Exe, mgr.Config{
		DisplayName: DisplayName,
		Description: DisplayName + " — background filesystem search indexer.",
		StartType:   mgr.StartAutomatic,
		// ServiceStartName left empty ⇒ LocalSystem (elevated; see the type comment).
	}, cfg.Args...)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// A Windows service takes no environment at create time — per-service env lives
	// in the registry as a REG_MULTI_SZ under the service key, which the SCM merges
	// into the process environment at launch. This is how FW_INDEX_ROOTS / FW_DATA_DIR
	// / FW_INDEX_ADDR reach the daemon.
	if len(cfg.Env) > 0 {
		if err := writeServiceEnv(Name, cfg.Env); err != nil {
			return fmt.Errorf("write service environment: %w", err)
		}
	}

	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

func (m scmManager) Uninstall() error {
	conn, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM (run elevated?): %w", err)
	}
	defer conn.Disconnect()

	s, err := conn.OpenService(Name)
	if err != nil {
		return nil // not installed
	}
	defer s.Close()

	// Request stop, then wait briefly for it to actually stop before deleting so the
	// service key is removed cleanly rather than being marked delete-pending.
	if st, err := s.Control(svc.Stop); err == nil {
		deadline := time.Now().Add(10 * time.Second)
		for st.State != svc.Stopped && time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
			if st, err = s.Query(); err != nil {
				break
			}
		}
	}
	return s.Delete()
}

func (m scmManager) Status() (State, error) {
	conn, err := mgr.Connect()
	if err != nil {
		return StateUnknown, fmt.Errorf("connect to SCM: %w", err)
	}
	defer conn.Disconnect()

	s, err := conn.OpenService(Name)
	if err != nil {
		return StateNotInstalled, nil
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return StateUnknown, err
	}
	if st.State == svc.Running || st.State == svc.StartPending {
		return StateRunning, nil
	}
	return StateStopped, nil
}

// writeServiceEnv sets the service's per-process environment (REG_MULTI_SZ
// "Environment" under HKLM\SYSTEM\CurrentControlSet\Services\<name>).
func writeServiceEnv(name string, env map[string]string) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Services\`+name, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringsValue("Environment", sortedEnv(env))
}
