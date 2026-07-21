//go:build windows

package indexer

import "os"

// SelectSource returns the Windows Source (see docs/INDEX.md, Phase 3).
//
// The native accelerator — NTFS **MFT** enumeration for a near-instant initial index
// (the "Everything" experience) and the **USN change journal** for whole-volume live
// updates with cross-restart catch-up (a persisted USN sequence, so a relaunch
// reconciles from the journal instead of re-walking) — is reachable from pure Go via
// golang.org/x/sys/windows + DeviceIoControl (FSCTL_QUERY_USN_JOURNAL /
// FSCTL_READ_USN_JOURNAL / FSCTL_ENUM_USN_DATA). It needs an elevated volume handle,
// so a small privileged helper may be warranted.
//
// The USN-journal watcher (with cross-restart cursor catch-up) is implemented in
// source_windows.go / usn_windows.go; the initial index still uses the portable walk
// (MFT enumeration is a further optimization). ⚠️ That native code is RUNTIME-UNTESTED
// (written against the Win32 docs, cross-compiled only) and falls back to the portable
// backend on any journal setup error, so the indexer works regardless.
func SelectSource(exclude ExcludeFunc, log func(string)) Source {
	// Escape hatch for the runtime-untested native backend: FW_INDEX_NATIVE=0 forces
	// the portable walk + fsnotify watcher, which is fully verified.
	if os.Getenv("FW_INDEX_NATIVE") == "0" {
		log("windows: native USN backend disabled (FW_INDEX_NATIVE=0), using portable")
		return NewPortableSource(exclude, log)
	}
	return newWindowsSource(exclude, log)
}
