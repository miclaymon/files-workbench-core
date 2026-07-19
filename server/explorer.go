package main

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

// explorerItem mirrors the Python make_item_info / _entry_to_item shape.
type explorerItem struct {
	Name          string             `json:"name"`
	Path          string             `json:"path"`
	Type          string             `json:"type"`
	Hidden        bool               `json:"hidden"`
	Icon          *string            `json:"icon"`
	IconOpen      *string            `json:"icon_open"`
	Customization *dirCustomization  `json:"customization,omitempty"`
	URI           *string            `json:"uri"`
	Size          *int64             `json:"size"`
	DateCreated   *float64           `json:"date_created"`
	DateModified  *float64           `json:"date_modified"`
	DateAccessed  *float64           `json:"date_accessed"`
}

func handleExplorerCategories(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]any{
		"categories": getAllBlacklistCategories(),
		"rules":      getAllBlacklistRules(),
	})
}

func handleExplorerRoot(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS == "windows" {
		jsonErr(w, http.StatusNotFound, "Not available on Windows")
		return
	}
	q := r.URL.Query()
	showHidden := qBool(q, "showHidden", true)
	showFiles := qBool(q, "showFiles", true)
	includeMetadata := qBool(q, "includeMetadata", false)
	excludeVals, hasExclude := q["excludeCategories"]
	excluded := parseExcludeParam(strings.Join(excludeVals, ","), hasExclude)

	path := "/"
	items := explorerListDir(path, excluded, showHidden, includeMetadata, showFiles)
	root := makeRootItem(path, "Root", "root")
	jsonOK(w, map[string]any{"root": root, "items": items})
}

func handleExplorerHome(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	showHidden := qBool(q, "showHidden", true)
	showFiles := qBool(q, "showFiles", true)
	includeMetadata := qBool(q, "includeMetadata", false)
	excludeVals, hasExclude := q["excludeCategories"]
	excluded := parseExcludeParam(strings.Join(excludeVals, ","), hasExclude)

	home, _ := os.UserHomeDir()
	items := explorerListDir(home, excluded, showHidden, includeMetadata, showFiles)
	root := makeItemInfo(home, "", "Home")
	jsonOK(w, map[string]any{"root": root, "items": items})
}

func handleExplorerDrives(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	showHidden := qBool(q, "showHidden", true)
	showFiles := qBool(q, "showFiles", true)
	excludeVals, hasExclude := q["excludeCategories"]
	excluded := parseExcludeParam(strings.Join(excludeVals, ","), hasExclude)

	switch runtime.GOOS {
	case "windows":
		// Windows: enumerate drive letters
		drives := windowsDrives(showHidden)
		root := explorerItem{Name: "Drives", Type: "drive", Hidden: false}
		jsonOK(w, map[string]any{"root": root, "items": drives})
	case "darwin":
		mounts := "/Volumes"
		if _, err := os.Stat(mounts); err != nil {
			root := makeItemInfo("/", "drive", "Volumes")
			jsonOK(w, map[string]any{"root": root, "items": []any{}})
			return
		}
		items := explorerListDir(mounts, excluded, showHidden, true, showFiles)
		root := makeItemInfo(mounts, "drive", "Volumes")
		jsonOK(w, map[string]any{"root": root, "items": items})
	default:
		// Linux: the Drives root *is* /mnt — list it like any directory so the
		// preloaded children match what expanding the node fetches
		// (`/Explorer?root=/mnt`). Parsing /proc/mounts here diverged from that
		// listing (it surfaced system mounts like btrfs subvolumes) and produced a
		// stale first-load tree that only corrected after a manual toggle. Removable
		// drives mounted under /mnt show up here directly.
		includeMetadata := qBool(q, "includeMetadata", false)
		mounts := "/mnt"
		items := explorerListDir(mounts, excluded, showHidden, includeMetadata, showFiles)
		root := makeItemInfo(mounts, "drive", "Drives")
		jsonOK(w, map[string]any{"root": root, "items": items})
	}
}

func handleExplorer(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("root")
	if path == "" {
		if runtime.GOOS == "windows" {
			jsonErr(w, http.StatusBadRequest, "root is required on Windows")
			return
		}
		path = "/"
	}
	if isExcluded(path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "Not found: "+path)
		return
	}
	if !info.IsDir() {
		jsonErr(w, http.StatusBadRequest, "Not a directory: "+path)
		return
	}

	showHidden := qBool(q, "showHidden", true)
	showFiles := qBool(q, "showFiles", true)
	includeMetadata := qBool(q, "includeMetadata", true)
	excludeVals, hasExclude := q["excludeCategories"]
	excluded := parseExcludeParam(strings.Join(excludeVals, ","), hasExclude)

	items := explorerListDir(path, excluded, showHidden, includeMetadata, showFiles)
	root := makeItemInfo(path, "", "")
	jsonOK(w, map[string]any{"root": root, "items": items})
}

// ── listing ───────────────────────────────────────────────────────────────────

func explorerListDir(dirPath string, excluded []string, showHidden, includeMetadata, showFiles bool) []explorerItem {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil
	}
	items := make([]explorerItem, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		hidden := strings.HasPrefix(name, ".")
		if !showHidden && hidden {
			continue
		}
		entryPath := filepath.Join(dirPath, name)
		if isExcluded(entryPath, excluded) {
			continue
		}

		eType := e.Type()
		var itemType string
		var symlinkTargetIsDir bool
		if eType&os.ModeSymlink != 0 {
			itemType = "symlink"
			// Stat the target so we can distinguish dir-symlinks from file-symlinks.
			if info, err := os.Stat(entryPath); err == nil {
				symlinkTargetIsDir = info.IsDir()
			}
		} else if e.IsDir() {
			itemType = "directory"
		} else if eType.IsRegular() {
			ext := strings.ToLower(filepath.Ext(name))
			if ext == ".lnk" {
				itemType = "shortcut"
			} else {
				mimeType := mime.TypeByExtension(ext)
				if mimeType == "" {
					mimeType = "application/octet-stream"
				} else if i := strings.Index(mimeType, ";"); i >= 0 {
					mimeType = strings.TrimSpace(mimeType[:i])
				}
				itemType = mimeType
			}
		} else {
			continue
		}

		// In tree mode (showFiles=false) keep only directories, directory-symlinks, and shortcuts.
		// File-symlinks are excluded the same as regular files.
		if !showFiles {
			isTreeDir := itemType == "directory" ||
				(itemType == "symlink" && symlinkTargetIsDir) ||
				itemType == "shortcut"
			if !isTreeDir {
				continue
			}
		}

		isDir := itemType == "directory" || (itemType == "symlink" && symlinkTargetIsDir)
		icon := activeIconTheme.resolve(name, isDir)
		var iconPtr, iconOpenPtr *string
		if icon != "" {
			iconPtr = &icon
		}
		var customization *dirCustomization
		if isDir {
			if open := activeIconTheme.resolveOpen(name); open != "" {
				iconOpenPtr = &open
			}
			customization = readDirCustomization(entryPath)
		}
		item := explorerItem{
			Name:          name,
			Path:          entryPath,
			Type:          itemType,
			Hidden:        hidden,
			Icon:          iconPtr,
			IconOpen:      iconOpenPtr,
			Customization: customization,
		}

		if includeMetadata {
			if info, err := e.Info(); err == nil {
				fillTimestamps(&item, info)
				if eType.IsRegular() {
					s := info.Size()
					item.Size = &s
				}
			}
		}

		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		aDir := a.Type == "directory" || a.Type == "drive" || a.Type == "root"
		bDir := b.Type == "directory" || b.Type == "drive" || b.Type == "root"
		if aDir != bDir {
			return aDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return items
}

func makeItemInfo(path, forceType, nameOverride string) explorerItem {
	info, err := os.Stat(path)
	name := nameOverride
	if name == "" && err == nil {
		name = info.Name()
		if name == "" {
			name = path
		}
	}
	itype := forceType
	if itype == "" && err == nil {
		if info.IsDir() {
			itype = "directory"
		} else {
			itype = "file"
		}
	}
	item := explorerItem{
		Name:   name,
		Path:   path,
		Type:   itype,
		Hidden: strings.HasPrefix(filepath.Base(path), "."),
	}
	if err == nil {
		fillTimestamps(&item, info)
		if info.Mode().IsRegular() {
			s := info.Size()
			item.Size = &s
		}
	}
	if item.Type == "directory" {
		item.Customization = readDirCustomization(path)
	}
	return item
}

func makeRootItem(path, name, itype string) explorerItem {
	return explorerItem{Name: name, Path: path, Type: itype, Hidden: false}
}

func fillTimestamps(item *explorerItem, info os.FileInfo) {
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		created := float64(sys.Ctim.Sec) + float64(sys.Ctim.Nsec)/1e9
		modified := float64(sys.Mtim.Sec) + float64(sys.Mtim.Nsec)/1e9
		accessed := float64(sys.Atim.Sec) + float64(sys.Atim.Nsec)/1e9
		item.DateCreated = &created
		item.DateModified = &modified
		item.DateAccessed = &accessed
	} else {
		m := float64(info.ModTime().UnixNano()) / 1e9
		item.DateModified = &m
	}
}

// ── drives ────────────────────────────────────────────────────────────────────

func windowsDrives(showHidden bool) []explorerItem {
	// Basic Windows drive enumeration without ctypes
	var items []explorerItem
	for c := 'A'; c <= 'Z'; c++ {
		drive := string(c) + ":\\"
		if info, err := os.Stat(drive); err == nil && info.IsDir() {
			item := makeItemInfo(drive, "drive", string(c)+":")
			if showHidden || !item.Hidden {
				items = append(items, item)
			}
		}
	}
	return items
}
