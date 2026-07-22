package indexer

import (
	"os"
	"path/filepath"
	"strings"
)

// statEntry builds an Entry from a live path (Lstat), or ok=false if it vanished.
// Shared by every native backend that reconciles a raw change record/event against
// current filesystem state (Windows USN, macOS FSEvents) — the record only proves
// something happened, so re-stating is what decides Added/Modified vs. Removed.
func statEntry(path, volume string) (Entry, bool) {
	info, err := os.Lstat(filepath.FromSlash(path))
	if err != nil {
		return Entry{}, false
	}
	return entryFromInfo(path, volume, info), true
}

// underAny reports whether path is one of, or nested under, one of roots.
func underAny(path string, roots []string) bool {
	for _, r := range roots {
		if path == r || strings.HasPrefix(path, r+"/") {
			return true
		}
	}
	return false
}
