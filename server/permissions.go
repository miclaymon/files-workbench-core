package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// protectedPaths is the set of absolute paths this application will never allow
// deletion of, regardless of OS-level permissions.
var protectedPaths map[string]bool

func init() {
	protectedPaths = buildProtectedPaths()
}

func buildProtectedPaths() map[string]bool {
	var paths []string
	switch runtime.GOOS {
	case "linux":
		paths = []string{
			"/", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
			"/usr", "/etc", "/proc", "/sys", "/dev", "/run", "/boot",
			"/root", "/home", "/snap", "/opt", "/srv",
		}
	case "darwin":
		paths = []string{
			"/", "/bin", "/sbin", "/usr", "/etc",
			"/System", "/Library", "/Applications",
			"/private", "/private/etc", "/private/var",
			"/cores", "/dev", "/Network", "/Volumes",
			"/usr/bin", "/usr/sbin", "/usr/lib",
		}
	case "windows":
		paths = []string{
			`C:\`, `C:\Windows`, `C:\Windows\System32`,
			`C:\Windows\SysWOW64`, `C:\Windows\WinSxS`,
			`C:\Program Files`, `C:\Program Files (x86)`,
			`C:\ProgramData`, `C:\Users`,
		}
	}
	m := make(map[string]bool, len(paths))
	for _, p := range paths {
		m[filepath.Clean(p)] = true
	}
	return m
}

// isProtectedPath returns true if the path is a protected system location that
// must never be deleted through this application. Checks:
//  1. Explicit protected-path list
//  2. On Unix: any path with depth ≤ 1 from root (e.g. /bin, /etc, /usr)
func isProtectedPath(path string) bool {
	p := filepath.Clean(path)
	if protectedPaths[p] {
		return true
	}
	// On Unix, block depth-1 paths even if not explicitly listed.
	// filepath.SplitList would give volume+dir; use separator count instead.
	if runtime.GOOS != "windows" {
		// p starts with "/" so count slashes: "/" → 1, "/bin" → 1, "/bin/ls" → 2
		if strings.Count(p, "/") <= 1 {
			return true
		}
	}
	return false
}

// isWritable checks whether the current process can write to dir by attempting
// to create and immediately remove a temporary file inside it.
func isWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".perm-check-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// permCheck is the result of checking delete permissions for a set of paths.
type permCheck struct {
	Protected         []string `json:"protected,omitempty"`
	RequiresElevation []string `json:"requires_elevation,omitempty"`
	ElevationMethod   string   `json:"elevation_method,omitempty"`
}

// elevationMethod returns the platform-appropriate method string.
func elevationMethod() string {
	switch runtime.GOOS {
	case "darwin":
		return "osascript"
	case "windows":
		return "windows_uac"
	default:
		return "sudo_password"
	}
}

// checkDeletePermissions inspects every path for protection and write access.
// Protected paths are always blocked. Non-writable but non-protected paths
// are flagged as requiring elevation.
func checkDeletePermissions(paths []string) permCheck {
	var result permCheck
	for _, p := range paths {
		if isProtectedPath(p) {
			result.Protected = append(result.Protected, p)
			continue
		}
		parent := filepath.Dir(p)
		if !isWritable(parent) {
			result.RequiresElevation = append(result.RequiresElevation, p)
		}
	}
	if len(result.RequiresElevation) > 0 {
		result.ElevationMethod = elevationMethod()
	}
	return result
}

// elevatedDelete permanently removes paths using OS-level privilege escalation.
// password is used only on Linux/other Unix (piped to sudo -S).
// On macOS, osascript triggers the native admin dialog; password is ignored.
func elevatedDelete(paths []string, password string) error {
	switch runtime.GOOS {
	case "darwin":
		return macosElevatedShell(paths, "rm -rf --")
	case "windows":
		return fmt.Errorf("Windows elevation requires restarting Files Workbench as Administrator")
	default:
		return sudoRm(paths, password)
	}
}

// elevatedTrash moves paths to the trash using elevated privileges.
// On Linux, elevated trash becomes a permanent delete (sudo rm) because the
// freedesktop trash dir is user-owned; you cannot sudo-move into another
// user's trash location cleanly.
func elevatedTrash(paths []string, password string) error {
	switch runtime.GOOS {
	case "darwin":
		// Finder's delete sends to the current user's trash even under admin.
		items := make([]string, len(paths))
		for i, p := range paths {
			items[i] = fmt.Sprintf(`(POSIX file %q)`, p)
		}
		script := fmt.Sprintf(`tell application "Finder" to delete {%s}`, strings.Join(items, ", "))
		out, err := exec.Command("osascript", "-e", script).CombinedOutput()
		if err != nil {
			return fmt.Errorf("osascript: %s", strings.TrimSpace(string(out)))
		}
		return nil
	case "windows":
		return fmt.Errorf("Windows elevation requires restarting Files Workbench as Administrator")
	default:
		// On Linux, elevated trash falls back to permanent delete.
		return sudoRm(paths, password)
	}
}

func sudoRm(paths []string, password string) error {
	// -S  read password from stdin
	// -p "" suppress the "Password:" prompt on stderr
	// -k  ignore cached credentials so we always use the supplied password
	args := append([]string{"-S", "-p", "", "-k", "--", "rm", "-rf", "--"}, paths...)
	cmd := exec.Command("sudo", args...)
	cmd.Stdin = strings.NewReader(password + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		// sudo prints nothing useful on auth failure; detect by exit code 1 with empty output.
		if strings.Contains(msg, "incorrect password") ||
			strings.Contains(msg, "Sorry, try again") ||
			strings.Contains(msg, "Authentication failure") {
			return fmt.Errorf("incorrect password")
		}
		return fmt.Errorf("sudo: %s", msg)
	}
	return nil
}

func macosElevatedShell(paths []string, baseCmd string) error {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = fmt.Sprintf("%q", p)
	}
	shellCmd := baseCmd + " " + strings.Join(quoted, " ")
	script := fmt.Sprintf(`do shell script %q with administrator privileges`, shellCmd)
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
