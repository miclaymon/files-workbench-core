//go:build darwin

package indexer

// SelectSource returns the macOS Source (see docs/INDEX.md, Phase 3).
//
// The native accelerator — **Spotlight** (NSMetadataQuery / mdfind), whose
// content+metadata index the OS already maintains, plus **FSEvents** for whole-tree
// live updates with cross-restart catch-up (a persisted event ID, so a relaunch
// resumes from the last-seen event instead of re-walking) — is reachable via cgo to
// the CoreServices/Metadata frameworks (or by shelling to mdfind/mdquery + an
// FSEvents stream). Note Spotlight can be disabled per-volume, so a portable
// fallback stays necessary.
//
// Until that lands, macOS uses the portable walk + FSEvents backend (via fsnotify).
// Implement newSpotlightSource behind this selector.
func SelectSource(exclude ExcludeFunc, log func(string)) Source {
	log("macos: portable backend (native Spotlight/FSEvents accelerator not yet implemented)")
	return NewPortableSource(exclude, log)
}
