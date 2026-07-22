//go:build darwin

package indexer

// SelectSource returns the macOS Source (see docs/INDEX.md, Phase 3, and
// docs/MACOS_BACKEND_PLAN.md for the full status and remaining steps).
//
// A real FSEvents-based backend (darwinSource, in source_darwin.go +
// fsevents_darwin.go) exists — portable Walk for the initial index, an FSEvents
// stream for whole-tree live updates with cross-restart catch-up via a persisted
// event ID (FSEvents keeps a genuine per-volume event log, so this is real
// catch-up, not a re-walk — the same idea as the Windows USN journal cursor). It is
// NOT wired in here. Unlike the Windows USN backend, it has never even been
// compiled: cgo to CoreServices needs CGO_ENABLED=1 plus the actual macOS SDK,
// neither of which exists on a non-Mac host, so there was no way to verify it at
// all before shipping this scaffolding. docs/MACOS_BACKEND_PLAN.md is the
// step-by-step guide to finishing, debugging, and wiring up newDarwinSource here —
// start with cmd/fsevents-smoke, not by flipping this function over blind.
//
// Spotlight (NSMetadataQuery/mdfind) was considered for the *initial* index (an
// MFT-enumeration equivalent) but isn't a clear win — it's cgo-only too, can be
// disabled per-volume, and doesn't cover excluded paths — see the plan doc's
// "Initial index" section for the tradeoff. Not attempted.
func SelectSource(exclude ExcludeFunc, log func(string)) Source {
	log("macos: portable backend (native FSEvents accelerator written but not wired — see docs/MACOS_BACKEND_PLAN.md)")
	return NewPortableSource(exclude, log)
}
