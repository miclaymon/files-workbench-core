package indexer

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// ExcludeFunc reports whether a path should be skipped. For a directory, returning
// true prunes the whole subtree. It is the caller's policy (e.g. built from the
// server's FW_BLACKLIST); the walker itself stays policy-free.
type ExcludeFunc func(path string, isDir bool) bool

// Walk enumerates root depth-first, calling emit for each entry that isn't excluded.
// Unreadable entries are skipped rather than aborting the walk — a permission error
// deep in a tree must not fail the whole index. Returns the count emitted.
//
// This is the portable Source: it works on every OS and is what v1 ships. Native
// backends (USN/MFT enumeration, Spotlight) replace it behind the same emit contract.
func Walk(root string, exclude ExcludeFunc, emit func(Entry) error) (int64, error) {
	if exclude == nil {
		exclude = func(string, bool) bool { return false }
	}
	volume := volumeID(root)
	var count int64

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we can't read: skip its subtree; a file: skip it. Never abort.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		isDir := d.IsDir()
		if exclude(path, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // vanished between readdir and stat — skip
		}
		if err := emit(entryFromInfo(path, volume, info)); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

// entryFromInfo builds a catalog Entry from a stat result. Shared by the walker
// and the watcher so a live update produces exactly the same row a full scan would.
func entryFromInfo(path, volume string, info fs.FileInfo) Entry {
	isDir := info.IsDir()
	e := Entry{
		Path:     path,
		Name:     info.Name(),
		Ext:      ext(info.Name(), isDir),
		IsDir:    isDir,
		VolumeID: volume,
		Modified: info.ModTime(),
		Created:  info.ModTime(), // best-effort until a native backend supplies birth time
	}
	if !isDir {
		e.Size = info.Size()
	}
	return e
}

// ext returns the lowercased extension without the leading dot ("" for dirs or
// names with no extension, including dotfiles like ".gitignore").
func ext(name string, isDir bool) string {
	if isDir {
		return ""
	}
	e := filepath.Ext(name)
	if e == "" || e == name { // "" or a bare dotfile (".bashrc")
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(e, "."))
}

// volumeID identifies the storage volume a root lives on. The portable
// implementation keys on the root path itself; native backends key on the real
// volume (NTFS volume GUID, macOS device id) so a single index can span drives.
func volumeID(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	return normPath(abs)
}

// DefaultExclude builds an ExcludeFunc that skips a set of directory base names
// (e.g. from the server's blacklist) anywhere in the tree, plus any path the OS
// reports as unreadable. Names match on the directory's base name only.
func DefaultExclude(skipNames []string) ExcludeFunc {
	set := make(map[string]struct{}, len(skipNames))
	for _, n := range skipNames {
		set[n] = struct{}{}
	}
	return func(path string, isDir bool) bool {
		if !isDir {
			return false
		}
		_, skip := set[filepath.Base(path)]
		return skip
	}
}
