//go:build windows

package indexer

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
// Until that lands, Windows uses the portable walk + ReadDirectoryChangesW backend
// (via fsnotify) — correct, but it re-walks on each start and is bounded by the
// per-directory watch model. Implement newUSNSource behind this selector.
func SelectSource(exclude ExcludeFunc, log func(string)) Source {
	log("windows: portable backend (native USN/MFT accelerator not yet implemented)")
	return NewPortableSource(exclude, log)
}
