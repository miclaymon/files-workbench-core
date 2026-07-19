package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ── dir-size cache ────────────────────────────────────────────────────────────
// getDirSize returns the cached byte total for path if the entry is younger
// than dirSizeTTL; otherwise it walks the tree, stores the result, and returns
// it. Handlers for mutations call invalidateDirSize so the next request re-walks.
//
// Long-term plan: replace with an incremental background index.

const dirSizeTTL = 5 * time.Minute

// dirSizeIdleAbort aborts a background walk when no client has polled its progress for
// this long — so navigating away from a huge directory doesn't leave a runaway walk.
const dirSizeIdleAbort = 5 * time.Second

// dirSizeJob is one directory's size computation: a background walk that atomically
// accumulates the running total while readers poll it. A finished job is cached (done)
// and reused until dirSizeTTL. `at` is written before `done` is set, and readers read
// it only after observing `done`, so the atomic store/load pair orders that access.
type dirSizeJob struct {
	size       atomic.Int64
	files      atomic.Int64
	done       atomic.Bool
	at         time.Time
	lastAccess atomic.Int64 // unix-nano of the most recent read, for idle-abort
}

var dirSizeCache sync.Map // path → *dirSizeJob
var errWalkAbort = errors.New("dir-size walk aborted (idle)")

// getDirSizeProgress returns path's current running total and whether the walk has
// finished. It starts a background walk on the first request (or after the cached
// result expires); concurrent/repeat callers share the one job and read its live
// counters. A completed (cached) path returns done=true immediately.
func getDirSizeProgress(path string) (size, files int64, done bool) {
	nowNano := time.Now().UnixNano()
	if v, ok := dirSizeCache.Load(path); ok {
		j := v.(*dirSizeJob)
		j.lastAccess.Store(nowNano)
		if j.done.Load() {
			if time.Since(j.at) < dirSizeTTL {
				return j.size.Load(), j.files.Load(), true
			}
			dirSizeCache.Delete(path) // stale — recompute below
		} else {
			return j.size.Load(), j.files.Load(), false
		}
	}
	job := &dirSizeJob{}
	job.lastAccess.Store(nowNano)
	actual, loaded := dirSizeCache.LoadOrStore(path, job)
	j := actual.(*dirSizeJob)
	if loaded {
		j.lastAccess.Store(nowNano) // someone else created it first
		return j.size.Load(), j.files.Load(), j.done.Load()
	}
	go walkDirSize(path, j)
	return 0, 0, false
}

func walkDirSize(path string, j *dirSizeJob) {
	var seen int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			j.size.Add(info.Size())
			j.files.Add(1)
		}
		if seen++; seen%2000 == 0 && time.Since(time.Unix(0, j.lastAccess.Load())) > dirSizeIdleAbort {
			return errWalkAbort
		}
		return nil
	})
	if err != nil { // aborted — drop the partial so a later request restarts cleanly
		dirSizeCache.Delete(path)
		return
	}
	j.at = time.Now()
	j.done.Store(true)
}

// getDirSize returns path's final size, blocking until the walk completes. For internal
// callers (simpleListDir's includeDirSize); reuses a cached complete job when present.
func getDirSize(path string) (size, files int64) {
	if v, ok := dirSizeCache.Load(path); ok {
		if j := v.(*dirSizeJob); j.done.Load() && time.Since(j.at) < dirSizeTTL {
			return j.size.Load(), j.files.Load()
		}
	}
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			size += info.Size()
			files++
		}
		return nil
	})
	j := &dirSizeJob{at: time.Now()}
	j.size.Store(size)
	j.files.Store(files)
	j.done.Store(true)
	dirSizeCache.Store(path, j)
	return
}

// invalidateDirSize drops the cache entry for each path and its immediate
// parent so the next size request re-walks the affected subtree.
func invalidateDirSize(paths ...string) {
	for _, p := range paths {
		dirSizeCache.Delete(p)
		dirSizeCache.Delete(filepath.Dir(p))
	}
}

// ─────────────────────────────────────────────────────────────────────────────

func handleFsStat(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
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
	kind := "file"
	var size *int64
	if info.IsDir() {
		kind = "dir"
	} else {
		s := info.Size()
		size = &s
	}
	mtime := info.ModTime().Format("2006-01-02T15:04:05")
	jsonOK(w, map[string]any{
		"name":  info.Name(),
		"path":  path,
		"kind":  kind,
		"size":  size,
		"mtime": mtime,
	})
}

func handleFsListDir(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
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
		jsonErr(w, http.StatusBadRequest, "Not a directory")
		return
	}

	showHidden := qBool(q, "showHidden", true)
	includeMetadata := qBool(q, "includeMetadata", true)
	includeDirSize := qBool(q, "includeDirSize", false)
	excludeVals, hasExclude := q["excludeCategories"]
	excluded := parseExcludeParam(strings.Join(excludeVals, ","), hasExclude)
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))

	items := simpleListDir(path, excluded, includeMetadata, showHidden)
	total := len(items)

	start := offset
	if start > total {
		start = total
	}
	var page []any
	if limit > 0 {
		end := start + limit
		if end > total {
			end = total
		}
		page = items[start:end]
	} else {
		page = items[start:]
	}

	// Compute directory sizes concurrently for the current page only.
	// Scoping to the page means at most PAGE_SIZE walks per request.
	if includeDirSize {
		const maxConcurrent = 8
		sem := make(chan struct{}, maxConcurrent)
		var wg sync.WaitGroup
		for _, raw := range page {
			m := raw.(map[string]any)
			if m["kind"] != "dir" {
				continue
			}
			p := m["path"].(string)
			wg.Add(1)
			go func(item map[string]any, dirPath string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				size, _ := getDirSize(dirPath)
				item["size"] = size
			}(m, p)
		}
		wg.Wait()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Expires", "0")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]any{"items": page, "total": total, "offset": offset})
}

var archiveExts = map[string]bool{
	".zip": true, ".tar": true, ".gz": true, ".bz2": true,
	".7z": true, ".rar": true, ".xz": true,
}
var archiveMultiExts = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tgz", ".tbz2"}

func isArchiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range archiveMultiExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return archiveExts[filepath.Ext(lower)]
}

func simpleListDir(dirPath string, excluded []string, includeMetadata, showHidden bool) []any {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil
	}
	pinned := readPinnedNames(dirPath)
	items := make([]any, 0, len(entries))
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
		kind := "other"
		if e.IsDir() {
			kind = "dir"
			// On macOS, .app bundles look like directories but should launch as applications
			if runtime.GOOS == "darwin" && strings.HasSuffix(strings.ToLower(name), ".app") {
				kind = "app"
			}
		} else if eType.IsRegular() || eType&os.ModeSymlink != 0 {
			kind = "file"
			if isArchiveName(name) {
				kind = "archive"
			}
		} else {
			continue
		}

		var size *int64
		var mtime *string
		if includeMetadata {
			if info, err := e.Info(); err == nil {
				if kind == "file" || kind == "archive" {
					s := info.Size()
					size = &s
				}
				m := info.ModTime().Format("2006-01-02T15:04:05")
				mtime = &m
			}
		}

		isDir := kind == "dir" || kind == "app"
		icon := activeIconTheme.resolve(name, isDir)
		var iconField, iconOpenField any
		if icon != "" {
			iconField = icon
		}
		var customization *dirCustomization
		if isDir {
			if open := activeIconTheme.resolveOpen(name); open != "" {
				iconOpenField = open
			}
			customization = readDirCustomization(entryPath)
		}
		item := map[string]any{
			"name":          name,
			"path":          entryPath,
			"kind":          kind,
			"size":          size,
			"mtime":         mtime,
			"hidden":        hidden,
			"icon":          iconField,
			"icon_open":     iconOpenField,
			"customization": customization,
		}
		if pinned[name] {
			item["pinned"] = true
		}
		items = append(items, item)
	}

	// Pinned items are grouped first; within the pinned group and within the rest the
	// usual rules apply (directories/apps/archives first, then case-insensitive name).
	sort.Slice(items, func(i, j int) bool {
		a := items[i].(map[string]any)
		b := items[j].(map[string]any)
		aPinned := a["pinned"] == true
		bPinned := b["pinned"] == true
		if aPinned != bPinned {
			return aPinned
		}
		aIsDir := a["kind"] == "dir" || a["kind"] == "app" || a["kind"] == "archive"
		bIsDir := b["kind"] == "dir" || b["kind"] == "app" || b["kind"] == "archive"
		if aIsDir != bIsDir {
			return aIsDir
		}
		return strings.ToLower(a["name"].(string)) < strings.ToLower(b["name"].(string))
	})
	return items
}

func handleFsDirSize(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	size, files, done := getDirSizeProgress(path)
	jsonOK(w, map[string]any{"size": size, "files": files, "done": done})
}

func handleFsPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	force := qBool(q, "force", false)

	info, err := os.Stat(path)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "Not found: "+path)
		return
	}
	if info.IsDir() {
		jsonOK(w, map[string]any{"kind": "dir"})
		return
	}

	maxBytes := loadMaxPreviewBytes()
	fileSize := info.Size()
	mimeType := guessMIME(path)

	if !force && fileSize > int64(maxBytes) {
		if !strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "video/") && !strings.HasPrefix(mimeType, "audio/") {
			jsonOK(w, map[string]any{"kind": "tooLarge", "fileSize": fileSize, "maxBytes": maxBytes})
			return
		}
	}

	readMax := maxBytes
	if force {
		readMax = int(fileSize) + 1
	}

	data, err := readFileBytes(path, readMax)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if strings.HasPrefix(mimeType, "image/") {
		b64 := base64.StdEncoding.EncodeToString(data)
		jsonOK(w, map[string]any{"kind": "image", "mime": mimeType, "dataUrl": "data:" + mimeType + ";base64," + b64})
		return
	}

	if utf8.Valid(data) && !hasBinaryBytes(data) {
		jsonOK(w, map[string]any{"kind": "text", "language": languageForPath(path), "text": string(data)})
	} else {
		jsonOK(w, map[string]any{"kind": "binary", "bytes": fileSize, "mime": mimeType})
	}
}

func handleFsOpenWithSystem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(body.Path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	if _, err := os.Stat(body.Path); err != nil {
		jsonErr(w, http.StatusNotFound, "Not found: "+body.Path)
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", body.Path)
	case "darwin":
		cmd = exec.Command("open", body.Path)
	default:
		cmd = exec.Command("xdg-open", body.Path)
	}
	if err := cmd.Start(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"path": body.Path})
}

func handleFsOpenTerminal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(body.Path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	info, err := os.Stat(body.Path)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "Not found: "+body.Path)
		return
	}
	dir := body.Path
	if !info.IsDir() {
		dir = filepath.Dir(body.Path)
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("wt", "-d", dir)
		if err := cmd.Start(); err != nil {
			// Fallback to cmd.exe
			cmd = exec.Command("cmd", "/c", "start", "cmd", "/K", "cd /d "+dir)
			if err2 := cmd.Start(); err2 != nil {
				jsonErr(w, http.StatusInternalServerError, err2.Error())
				return
			}
		}
		jsonOK(w, map[string]string{"dir": dir})
		return
	case "darwin":
		// AppleScript to open Terminal.app at the given directory
		script := `tell application "Terminal" to do script "cd ` + strings.ReplaceAll(dir, `"`, `\"`) + `"`
		cmd = exec.Command("osascript", "-e", script)
	default:
		// Try common Linux terminal emulators in priority order
		type termSpec struct {
			bin  string
			args []string
		}
		candidates := []termSpec{
			{"x-terminal-emulator", []string{"--working-directory=" + dir}},
			{"gnome-terminal", []string{"--working-directory=" + dir}},
			{"konsole", []string{"--workdir", dir}},
			{"xfce4-terminal", []string{"--working-directory=" + dir}},
			{"mate-terminal", []string{"--working-directory=" + dir}},
			{"tilix", []string{"--working-directory=" + dir}},
			{"alacritty", []string{"--working-directory", dir}},
			{"kitty", []string{"-d", dir}},
			{"wezterm", []string{"start", "--cwd", dir}},
			{"xterm", []string{"-e", "bash -c 'cd \"" + dir + "\" && exec bash'"}},
		}
		for _, t := range candidates {
			if binPath, lookErr := exec.LookPath(t.bin); lookErr == nil {
				cmd = exec.Command(binPath, t.args...)
				break
			}
		}
		if cmd == nil {
			jsonErr(w, http.StatusNotFound, "no terminal emulator found")
			return
		}
	}

	if err := cmd.Start(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"dir": dir})
}

func handleFsCreateFile(w http.ResponseWriter, r *http.Request) {
	var body struct{ Path string `json:"path"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(body.Path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	if _, err := os.Stat(body.Path); err == nil {
		jsonErr(w, http.StatusConflict, "Already exists")
		return
	}
	if err := os.MkdirAll(filepath.Dir(body.Path), 0755); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, err := os.Create(body.Path)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	f.Close()
	invalidateDirSize(body.Path)
	jsonOK(w, map[string]string{"path": body.Path})
}

func handleFsCreateDir(w http.ResponseWriter, r *http.Request) {
	var body struct{ Path string `json:"path"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(body.Path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	if _, err := os.Stat(body.Path); err == nil {
		jsonErr(w, http.StatusConflict, "Already exists")
		return
	}
	if err := os.MkdirAll(body.Path, 0755); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	invalidateDirSize(body.Path)
	jsonOK(w, map[string]string{"path": body.Path})
}

func handleFsWriteFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(body.Path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	if err := os.MkdirAll(filepath.Dir(body.Path), 0755); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(body.Path, []byte(body.Content), 0644); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"path": body.Path})
}

// ── file operations ───────────────────────────────────────────────────────────

func handleFsRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		NewName string `json:"new_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" || body.NewName == "" {
		jsonErr(w, http.StatusBadRequest, "path and new_name required")
		return
	}
	if isExcluded(body.Path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	if strings.ContainsAny(body.NewName, "/\\") {
		jsonErr(w, http.StatusBadRequest, "new_name must not contain path separators")
		return
	}
	if _, err := os.Stat(body.Path); err != nil {
		jsonErr(w, http.StatusNotFound, "Not found: "+body.Path)
		return
	}
	newPath := filepath.Join(filepath.Dir(body.Path), body.NewName)
	if _, err := os.Stat(newPath); err == nil {
		jsonErr(w, http.StatusConflict, "Destination already exists: "+newPath)
		return
	}
	if err := os.Rename(body.Path, newPath); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	invalidateDirSize(body.Path, newPath)
	jsonOK(w, map[string]string{"path": newPath, "old_path": body.Path})
}

func handleFsMove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths   []string `json:"paths"`
		DestDir string   `json:"dest_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Paths) == 0 || body.DestDir == "" {
		jsonErr(w, http.StatusBadRequest, "paths and dest_dir required")
		return
	}
	if isExcluded(body.DestDir, nil) {
		jsonErr(w, http.StatusForbidden, "Destination is blacklisted")
		return
	}
	if err := os.MkdirAll(body.DestDir, 0755); err != nil {
		jsonErr(w, http.StatusInternalServerError, "Cannot create destination: "+err.Error())
		return
	}
	moved := make([]map[string]string, 0, len(body.Paths))
	for _, src := range body.Paths {
		if isExcluded(src, nil) {
			jsonErr(w, http.StatusForbidden, "Source is blacklisted: "+src)
			return
		}
		if _, err := os.Stat(src); err != nil {
			jsonErr(w, http.StatusNotFound, "Not found: "+src)
			return
		}
		dst := filepath.Join(body.DestDir, filepath.Base(src))
		if err := os.Rename(src, dst); err != nil {
			// Cross-device move: copy then delete
			if err2 := copyPath(src, dst); err2 != nil {
				jsonErr(w, http.StatusInternalServerError, fmt.Sprintf("move %s: %v", filepath.Base(src), err2))
				return
			}
			if err2 := os.RemoveAll(src); err2 != nil {
				jsonErr(w, http.StatusInternalServerError, fmt.Sprintf("remove after copy %s: %v", filepath.Base(src), err2))
				return
			}
		}
		moved = append(moved, map[string]string{"old_path": src, "path": dst})
	}
	toInvalidate := make([]string, 0, len(moved)*2)
	for _, m := range moved {
		toInvalidate = append(toInvalidate, m["old_path"], m["path"])
	}
	invalidateDirSize(toInvalidate...)
	jsonOK(w, map[string]any{"moved": moved})
}

func handleFsCopy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths   []string `json:"paths"`
		DestDir string   `json:"dest_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Paths) == 0 || body.DestDir == "" {
		jsonErr(w, http.StatusBadRequest, "paths and dest_dir required")
		return
	}
	if isExcluded(body.DestDir, nil) {
		jsonErr(w, http.StatusForbidden, "Destination is blacklisted")
		return
	}
	if err := os.MkdirAll(body.DestDir, 0755); err != nil {
		jsonErr(w, http.StatusInternalServerError, "Cannot create destination: "+err.Error())
		return
	}
	copied := make([]map[string]string, 0, len(body.Paths))
	for _, src := range body.Paths {
		if isExcluded(src, nil) {
			jsonErr(w, http.StatusForbidden, "Source is blacklisted: "+src)
			return
		}
		dst := filepath.Join(body.DestDir, filepath.Base(src))
		// Avoid overwrite: append " (copy)" suffix
		if _, err := os.Stat(dst); err == nil {
			ext := filepath.Ext(filepath.Base(src))
			base := strings.TrimSuffix(filepath.Base(src), ext)
			dst = filepath.Join(body.DestDir, base+" (copy)"+ext)
		}
		if err := copyPath(src, dst); err != nil {
			jsonErr(w, http.StatusInternalServerError, fmt.Sprintf("copy %s: %v", filepath.Base(src), err))
			return
		}
		copied = append(copied, map[string]string{"old_path": src, "path": dst})
	}
	toInvalidateCopy := make([]string, 0, len(copied))
	for _, c := range copied {
		toInvalidateCopy = append(toInvalidateCopy, c["path"])
	}
	invalidateDirSize(toInvalidateCopy...)
	jsonOK(w, map[string]any{"copied": copied})
}

func handleFsDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Paths) == 0 {
		jsonErr(w, http.StatusBadRequest, "paths required")
		return
	}
	for _, p := range body.Paths {
		if isExcluded(p, nil) {
			jsonErr(w, http.StatusForbidden, "Path is blacklisted: "+p)
			return
		}
	}
	perm := checkDeletePermissions(body.Paths)
	if len(perm.Protected) > 0 {
		jsonErr(w, http.StatusForbidden, "Protected system path: "+strings.Join(perm.Protected, ", "))
		return
	}
	if len(perm.RequiresElevation) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"error":            "requires_elevation",
			"elevation_method": perm.ElevationMethod,
			"paths":            perm.RequiresElevation,
		})
		return
	}
	for _, p := range body.Paths {
		if err := os.RemoveAll(p); err != nil {
			jsonErr(w, http.StatusInternalServerError, fmt.Sprintf("delete %s: %v", filepath.Base(p), err))
			return
		}
	}
	invalidateDirSize(body.Paths...)
	jsonOK(w, map[string]any{"deleted": body.Paths})
}

func handleFsDeleteElevated(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths    []string `json:"paths"`
		Password string   `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Paths) == 0 {
		jsonErr(w, http.StatusBadRequest, "paths required")
		return
	}
	for _, p := range body.Paths {
		if isExcluded(p, nil) {
			jsonErr(w, http.StatusForbidden, "Path is blacklisted: "+p)
			return
		}
		if isProtectedPath(p) {
			jsonErr(w, http.StatusForbidden, "Protected system path: "+p)
			return
		}
	}
	if err := elevatedDelete(body.Paths, body.Password); err != nil {
		if err.Error() == "incorrect password" {
			jsonErr(w, http.StatusUnauthorized, "Incorrect password")
			return
		}
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	invalidateDirSize(body.Paths...)
	jsonOK(w, map[string]any{"deleted": body.Paths})
}

func handleFsTrash(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Paths) == 0 {
		jsonErr(w, http.StatusBadRequest, "paths required")
		return
	}
	for _, p := range body.Paths {
		if isExcluded(p, nil) {
			jsonErr(w, http.StatusForbidden, "Path is blacklisted: "+p)
			return
		}
		if _, err := os.Stat(p); err != nil {
			jsonErr(w, http.StatusNotFound, "Not found: "+p)
			return
		}
	}
	perm := checkDeletePermissions(body.Paths)
	if len(perm.Protected) > 0 {
		jsonErr(w, http.StatusForbidden, "Protected system path: "+strings.Join(perm.Protected, ", "))
		return
	}
	if len(perm.RequiresElevation) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"error":            "requires_elevation",
			"elevation_method": perm.ElevationMethod,
			"paths":            perm.RequiresElevation,
		})
		return
	}
	for _, p := range body.Paths {
		if err := trashPath(p); err != nil {
			jsonErr(w, http.StatusInternalServerError, fmt.Sprintf("trash %s: %v", filepath.Base(p), err))
			return
		}
	}
	invalidateDirSize(body.Paths...)
	jsonOK(w, map[string]any{"trashed": body.Paths})
}

func handleFsTrashElevated(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths    []string `json:"paths"`
		Password string   `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Paths) == 0 {
		jsonErr(w, http.StatusBadRequest, "paths required")
		return
	}
	for _, p := range body.Paths {
		if isExcluded(p, nil) {
			jsonErr(w, http.StatusForbidden, "Path is blacklisted: "+p)
			return
		}
		if isProtectedPath(p) {
			jsonErr(w, http.StatusForbidden, "Protected system path: "+p)
			return
		}
	}
	if err := elevatedTrash(body.Paths, body.Password); err != nil {
		if err.Error() == "incorrect password" {
			jsonErr(w, http.StatusUnauthorized, "Incorrect password")
			return
		}
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	invalidateDirSize(body.Paths...)
	jsonOK(w, map[string]any{"trashed": body.Paths})
}

// trashPath moves a path to the OS trash / recycle bin.
func trashPath(path string) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`tell application "Finder" to delete POSIX file %q`, path)
		return exec.Command("osascript", "-e", script).Run()
	case "windows":
		ps := fmt.Sprintf(`Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteFile(%q,'OnlyErrorDialogs','SendToRecycleBin')`, path)
		return exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
	default:
		return trashFreedesktop(path)
	}
}

// trashFreedesktop implements the freedesktop.org Trash specification for Linux.
func trashFreedesktop(path string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	trashDir := filepath.Join(home, ".local", "share", "Trash")
	filesDir := filepath.Join(trashDir, "files")
	infoDir := filepath.Join(trashDir, "info")
	for _, d := range []string{filesDir, infoDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}

	base := filepath.Base(path)
	dest := filepath.Join(filesDir, base)
	// Avoid collision by appending a counter.
	if _, err := os.Stat(dest); err == nil {
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		for i := 2; ; i++ {
			candidate := filepath.Join(filesDir, fmt.Sprintf("%s_%d%s", stem, i, ext))
			if _, err2 := os.Stat(candidate); os.IsNotExist(err2) {
				dest = candidate
				base = filepath.Base(dest)
				break
			}
		}
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	infoContent := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n",
		abs, time.Now().Format("2006-01-02T15:04:05"))
	infoFile := filepath.Join(infoDir, base+".trashinfo")
	if err := os.WriteFile(infoFile, []byte(infoContent), 0600); err != nil {
		return err
	}

	if err := os.Rename(path, dest); err != nil {
		// Cross-device: copy + delete
		if err2 := copyPath(path, dest); err2 != nil {
			os.Remove(infoFile)
			return err2
		}
		if err2 := os.RemoveAll(path); err2 != nil {
			os.Remove(infoFile)
			os.RemoveAll(dest)
			return err2
		}
	}
	return nil
}

// copyPath copies a file or directory tree from src to dst.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(src, dst, info.Mode())
	}
	return copyFile(src, dst, info.Mode())
}

func copyDir(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(dst, mode); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			info, err := e.Info()
			if err != nil {
				return err
			}
			if err := copyDir(s, d, info.Mode()); err != nil {
				return err
			}
		} else {
			info, err := e.Info()
			if err != nil {
				return err
			}
			if err := copyFile(s, d, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ── utilities ─────────────────────────────────────────────────────────────────

func qBool(q url.Values, key string, def bool) bool {
	v := q.Get(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func guessMIME(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	t := mime.TypeByExtension(ext)
	if t == "" {
		return "application/octet-stream"
	}
	if i := strings.Index(t, ";"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

func readFileBytes(path string, maxBytes int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, maxBytes)
	n, _ := f.Read(buf)
	return buf[:n], nil
}

func hasBinaryBytes(data []byte) bool {
	check := data
	if len(check) > 512 {
		check = check[:512]
	}
	for _, b := range check {
		if b == 0 {
			return true
		}
	}
	return false
}

func languageForPath(path string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	m := map[string]string{
		"js": "javascript", "mjs": "javascript", "cjs": "javascript",
		"ts": "typescript", "tsx": "typescript",
		"py": "python", "json": "json", "md": "markdown",
		"html": "html", "htm": "html", "css": "css",
		"rs": "rust", "cpp": "cpp", "cc": "cpp", "cxx": "cpp", "h": "cpp", "hpp": "cpp",
		"vue": "html", "yaml": "yaml", "yml": "yaml", "sh": "shell", "go": "go",
	}
	if lang, ok := m[ext]; ok {
		return lang
	}
	return "plaintext"
}

func loadMaxPreviewBytes() int {
	defaults := loadJSONFile(configPrefsPath("default-preferences.json"))
	user := loadJSONFile(userPrefsPath())
	if v := nestedInt(user, "preview", "maxPreviewBytes"); v > 0 {
		return v
	}
	if v := nestedInt(defaults, "preview", "maxPreviewBytes"); v > 0 {
		return v
	}
	return 10000
}

func loadJSONFile(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	return m
}

func nestedInt(m map[string]any, keys ...string) int {
	cur := m
	for i, k := range keys {
		if cur == nil {
			return 0
		}
		v := cur[k]
		if i == len(keys)-1 {
			switch n := v.(type) {
			case float64:
				return int(n)
			case int:
				return n
			}
			return 0
		}
		next, ok := v.(map[string]any)
		if !ok {
			return 0
		}
		cur = next
	}
	return 0
}

// isoTime formats a time as ISO 8601 without timezone (matches Python's isoformat(timespec="seconds")).
func isoTime(t time.Time) string {
	return t.Format("2006-01-02T15:04:05")
}
