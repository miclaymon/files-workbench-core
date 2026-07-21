//go:build !windows && !darwin

package indexer

// SelectSource returns the best available Source for the current OS (see
// docs/INDEX.md, Phase 3). On Linux and other Unix, that is the portable
// walk + fsnotify backend: there is no unprivileged whole-filesystem accelerator
// (fanotify needs CAP_SYS_ADMIN, and the indexer runs as the user), so the portable
// Source is the intended production backend here.
//
// Windows and macOS override this in their own build-tagged files, where the native
// accelerators (USN/MFT, Spotlight/FSEvents) slot in behind the same interface.
func SelectSource(exclude ExcludeFunc, log func(string)) Source {
	return NewPortableSource(exclude, log)
}
