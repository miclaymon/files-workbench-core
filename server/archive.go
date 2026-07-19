package main

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ─── capabilities ─────────────────────────────────────────────────────────────

type archiveTool struct {
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
}

type archiveCapabilities struct {
	// Always available (Go native)
	Zip   bool `json:"zip"`
	Tar   bool `json:"tar"`
	TarGz bool `json:"tar_gz"`
	TarBz2 bool `json:"tar_bz2"` // extraction only (stdlib bzip2 is read-only)

	// External tools
	SevenZip archiveTool `json:"seven_zip"` // 7z or 7za
	Rar      archiveTool `json:"rar"`       // rar (create) or unrar (extract)
	Unrar    archiveTool `json:"unrar"`
}

func handleArchiveCapabilities(w http.ResponseWriter, r *http.Request) {
	sevPath, _ := exec.LookPath("7z")
	if sevPath == "" {
		sevPath, _ = exec.LookPath("7za")
	}
	rarPath, _ := exec.LookPath("rar")
	unrarPath, _ := exec.LookPath("unrar")

	caps := archiveCapabilities{
		Zip:    true,
		Tar:    true,
		TarGz:  true,
		TarBz2: true,
		SevenZip: archiveTool{Available: sevPath != "", Path: sevPath},
		Rar:      archiveTool{Available: rarPath != "", Path: rarPath},
		Unrar:    archiveTool{Available: unrarPath != "", Path: unrarPath},
	}
	jsonOK(w, caps)
}

// ─── compress ─────────────────────────────────────────────────────────────────

func handleFsCompress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths  []string `json:"paths"`
		Format string   `json:"format"` // "zip" | "tar" | "tar.gz" | "7z"
		Dest   string   `json:"dest"`   // full output path including filename
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		len(body.Paths) == 0 || body.Format == "" || body.Dest == "" {
		jsonErr(w, http.StatusBadRequest, "paths, format, and dest required")
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
	if isExcluded(body.Dest, nil) {
		jsonErr(w, http.StatusForbidden, "Destination is blacklisted")
		return
	}

	var err error
	switch body.Format {
	case "zip":
		err = compressZip(body.Paths, body.Dest)
	case "tar":
		err = compressTar(body.Paths, body.Dest, "")
	case "tar.gz", "tgz":
		err = compressTar(body.Paths, body.Dest, "gz")
	case "7z":
		err = compress7z(body.Paths, body.Dest)
	default:
		jsonErr(w, http.StatusBadRequest, "unsupported format: "+body.Format)
		return
	}
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"dest": body.Dest})
}

// ─── decompress ───────────────────────────────────────────────────────────────

func handleFsDecompress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		DestDir string `json:"dest_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" || body.DestDir == "" {
		jsonErr(w, http.StatusBadRequest, "path and dest_dir required")
		return
	}
	if isExcluded(body.Path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	if _, err := os.Stat(body.Path); err != nil {
		jsonErr(w, http.StatusNotFound, "Archive not found: "+body.Path)
		return
	}
	if err := os.MkdirAll(body.DestDir, 0755); err != nil {
		jsonErr(w, http.StatusInternalServerError, "Cannot create destination: "+err.Error())
		return
	}

	ext := archiveExt(body.Path)
	var err error
	switch ext {
	case ".zip":
		err = extractZip(body.Path, body.DestDir)
	case ".tar":
		err = extractTar(body.Path, body.DestDir, "")
	case ".tar.gz", ".tgz":
		err = extractTar(body.Path, body.DestDir, "gz")
	case ".tar.bz2", ".tbz2":
		err = extractTar(body.Path, body.DestDir, "bz2")
	case ".tar.xz", ".txz":
		err = extract7z(body.Path, body.DestDir)
	case ".7z":
		err = extract7z(body.Path, body.DestDir)
	case ".rar":
		err = extractRar(body.Path, body.DestDir)
	case ".gz":
		err = extractGzSingle(body.Path, body.DestDir)
	default:
		// Try 7z as a universal fallback
		if err2 := extract7z(body.Path, body.DestDir); err2 != nil {
			jsonErr(w, http.StatusBadRequest, "unsupported archive format: "+ext)
			return
		}
	}
	if err != nil {
		os.Remove(body.DestDir) // clean up empty dest if we created it and failed
		if isToolMissing(err) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{
				"error":       "missing_tool",
				"tool":        requiredTool(ext),
				"detail":      err.Error(),
				"install_apt": installCmd("apt", ext),
				"install_dnf": installCmd("dnf", ext),
				"install_pac": installCmd("pacman", ext),
				"install_brew": installCmd("brew", ext),
			})
			return
		}
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"dest_dir": body.DestDir})
}

// ─── zip ──────────────────────────────────────────────────────────────────────

func compressZip(paths []string, dest string) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	for _, p := range paths {
		baseDir := filepath.Dir(p)
		if err := addToZip(zw, p, baseDir); err != nil {
			return err
		}
	}
	return nil
}

func addToZip(zw *zip.Writer, path, baseDir string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(baseDir, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)

	if info.IsDir() {
		// Ensure directory entry ends with "/"
		if _, err := zw.Create(rel + "/"); err != nil {
			return err
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := addToZip(zw, filepath.Join(path, e.Name()), baseDir); err != nil {
				return err
			}
		}
		return nil
	}

	fw, err := zw.Create(rel)
	if err != nil {
		return err
	}
	rf, err := os.Open(path)
	if err != nil {
		return err
	}
	defer rf.Close()
	_, err = io.Copy(fw, rf)
	return err
}

func extractZip(src, destDir string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	for _, f := range r.File {
		target := filepath.Join(destDir, filepath.FromSlash(f.Name))
		// Prevent ZIP slip (path traversal)
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanDest) &&
			filepath.Clean(target) != filepath.Clean(destDir) {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, f.Mode())
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		out.Close()
		rc.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}

// ─── tar ──────────────────────────────────────────────────────────────────────

// compression: "" = none, "gz" = gzip, "bz2" = bzip2
func compressTar(paths []string, dest, compression string) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	var tw *tar.Writer
	switch compression {
	case "gz":
		gw := gzip.NewWriter(f)
		defer gw.Close()
		tw = tar.NewWriter(gw)
	default:
		tw = tar.NewWriter(f)
	}
	defer tw.Close()

	for _, p := range paths {
		baseDir := filepath.Dir(p)
		if err := addToTar(tw, p, baseDir); err != nil {
			return err
		}
	}
	return nil
}

func addToTar(tw *tar.Writer, path, baseDir string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(baseDir, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)

	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = rel
	if info.IsDir() {
		hdr.Name += "/"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := addToTar(tw, filepath.Join(path, e.Name()), baseDir); err != nil {
				return err
			}
		}
		return nil
	}
	rf, err := os.Open(path)
	if err != nil {
		return err
	}
	defer rf.Close()
	_, err = io.Copy(tw, rf)
	return err
}

func extractTar(src, destDir, compression string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	var tr *tar.Reader
	switch compression {
	case "gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gr.Close()
		tr = tar.NewReader(gr)
	case "bz2":
		tr = tar.NewReader(bzip2.NewReader(f))
	default:
		tr = tar.NewReader(f)
	}

	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.FromSlash(hdr.Name))
		// Prevent tar slip
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanDest) &&
			filepath.Clean(target) != filepath.Clean(destDir) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, os.FileMode(hdr.Mode))
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			out.Close()
			if copyErr != nil {
				return copyErr
			}
		}
	}
	return nil
}

func extractGzSingle(src, destDir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	// Output filename: strip the .gz extension
	base := filepath.Base(src)
	outName := strings.TrimSuffix(base, ".gz")
	if outName == base {
		outName = base + ".out"
	}
	out, err := os.Create(filepath.Join(destDir, outName))
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, gr)
	return err
}

// ─── 7z ───────────────────────────────────────────────────────────────────────

func sevenZipBin() (string, error) {
	for _, name := range []string{"7z", "7za"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("7z not found: install p7zip-full (apt), p7zip (pacman/dnf/brew)")
}

func compress7z(paths []string, dest string) error {
	bin, err := sevenZipBin()
	if err != nil {
		return err
	}
	args := append([]string{"a", dest, "--"}, paths...)
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("7z: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func extract7z(src, destDir string) error {
	bin, err := sevenZipBin()
	if err != nil {
		return err
	}
	out, err := exec.Command(bin, "x", "-o"+destDir, "-y", "--", src).CombinedOutput()
	if err != nil {
		return fmt.Errorf("7z: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ─── rar ──────────────────────────────────────────────────────────────────────

func extractRar(src, destDir string) error {
	// Try unrar first, then 7z
	if p, err := exec.LookPath("unrar"); err == nil {
		out, err := exec.Command(p, "x", "-y", src, destDir+string(os.PathSeparator)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("unrar: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := extract7z(src, destDir); err == nil {
		return nil
	}
	return fmt.Errorf("unrar not found: install unrar (apt/dnf/brew) or p7zip-full")
}

// ─── archive listing ──────────────────────────────────────────────────────────

type archiveLsEntry struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // "file" | "dir"
	Size  *int64 `json:"size,omitempty"`
	Mtime string `json:"mtime,omitempty"`
}

// GET /fs/archive/ls?path=...&inner=...
// inner is the directory inside the archive to list, e.g. "" = root, "docs/" = inside docs.
func handleFsArchiveLs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	archivePath := q.Get("path")
	innerPath := q.Get("inner")
	if archivePath == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(archivePath, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	if _, err := os.Stat(archivePath); err != nil {
		jsonErr(w, http.StatusNotFound, "Archive not found: "+archivePath)
		return
	}

	// Normalize: no leading slash, trailing slash required (or empty for root)
	innerPath = strings.TrimPrefix(innerPath, "/")
	if innerPath != "" && !strings.HasSuffix(innerPath, "/") {
		innerPath += "/"
	}

	ext := archiveExt(archivePath)
	var all []archiveLsEntry
	var err error
	switch ext {
	case ".zip":
		all, err = lsZip(archivePath)
	case ".tar":
		all, err = lsTar(archivePath, "")
	case ".tar.gz", ".tgz":
		all, err = lsTar(archivePath, "gz")
	case ".tar.bz2", ".tbz2":
		all, err = lsTar(archivePath, "bz2")
	case ".7z":
		all, err = ls7z(archivePath)
	case ".rar":
		all, err = lsRar(archivePath)
	default:
		if a, e := ls7z(archivePath); e == nil {
			all = a
		} else {
			jsonErr(w, http.StatusBadRequest, "unsupported archive format: "+ext)
			return
		}
	}
	if err != nil {
		if isToolMissing(err) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{
				"error": "missing_tool",
				"tool":  requiredTool(ext),
			})
			return
		}
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := filterArchiveEntries(all, innerPath)
	jsonOK(w, map[string]any{"items": items})
}

// filterArchiveEntries returns direct children of innerPath from a flat entry list.
// innerPath must be "" or end with "/". Returns name-only entries (just the filename/dirname).
func filterArchiveEntries(all []archiveLsEntry, innerPath string) []archiveLsEntry {
	seen := make(map[string]bool)
	var result []archiveLsEntry
	for _, e := range all {
		if !strings.HasPrefix(e.Name, innerPath) {
			continue
		}
		rest := e.Name[len(innerPath):]
		if rest == "" {
			continue // the directory entry for innerPath itself
		}
		slashIdx := strings.Index(rest, "/")
		if slashIdx == -1 {
			// Direct child (file or explicit dir)
			if !seen[rest] {
				seen[rest] = true
				result = append(result, archiveLsEntry{Name: rest, Kind: e.Kind, Size: e.Size, Mtime: e.Mtime})
			}
		} else {
			// Implied or nested: first segment is a subdirectory
			dirName := rest[:slashIdx]
			if !seen[dirName] {
				seen[dirName] = true
				result = append(result, archiveLsEntry{Name: dirName, Kind: "dir"})
			}
		}
	}
	return result
}

func lsZip(src string) ([]archiveLsEntry, error) {
	r, err := zip.OpenReader(src)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var entries []archiveLsEntry
	for _, f := range r.File {
		name := strings.TrimPrefix(f.Name, "./")
		kind := "file"
		if f.FileInfo().IsDir() {
			kind = "dir"
			name = strings.TrimSuffix(name, "/")
		}
		size := int64(f.UncompressedSize64)
		mtime := f.Modified.Format("2006-01-02T15:04:05")
		entries = append(entries, archiveLsEntry{Name: name, Kind: kind, Size: &size, Mtime: mtime})
	}
	return entries, nil
}

func lsTar(src, compression string) ([]archiveLsEntry, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var tr *tar.Reader
	switch compression {
	case "gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		tr = tar.NewReader(gr)
	case "bz2":
		tr = tar.NewReader(bzip2.NewReader(f))
	default:
		tr = tar.NewReader(f)
	}
	var entries []archiveLsEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		kind := "file"
		if hdr.Typeflag == tar.TypeDir {
			kind = "dir"
			name = strings.TrimSuffix(name, "/")
		}
		size := hdr.Size
		mtime := hdr.ModTime.Format("2006-01-02T15:04:05")
		entries = append(entries, archiveLsEntry{Name: name, Kind: kind, Size: &size, Mtime: mtime})
	}
	return entries, nil
}

func ls7z(src string) ([]archiveLsEntry, error) {
	bin, err := sevenZipBin()
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(bin, "l", "-slt", "--", src).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("7z: %s", strings.TrimSpace(string(out)))
	}
	return parse7zListing(string(out)), nil
}

func parse7zListing(output string) []archiveLsEntry {
	var entries []archiveLsEntry
	for _, block := range strings.Split(output, "----------") {
		var name, mtime string
		var size int64
		isFolder := false
		hasPath := false
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			before, after, ok := strings.Cut(line, " = ")
			if !ok {
				continue
			}
			key, val := strings.TrimSpace(before), strings.TrimSpace(after)
			switch key {
			case "Path":
				name = val
				hasPath = true
			case "Folder":
				isFolder = val == "+"
			case "Size":
				size, _ = strconv.ParseInt(val, 10, 64)
			case "Modified":
				// "2023-01-01 12:00:00" → ISO-ish
				mtime = strings.ReplaceAll(val, " ", "T")
			}
		}
		if !hasPath {
			continue
		}
		e := archiveLsEntry{Mtime: mtime}
		if isFolder {
			e.Kind = "dir"
			e.Name = strings.TrimSuffix(name, "/")
		} else {
			e.Kind = "file"
			e.Name = name
			e.Size = &size
		}
		entries = append(entries, e)
	}
	return entries
}

func lsRar(src string) ([]archiveLsEntry, error) {
	if p, err := exec.LookPath("unrar"); err == nil {
		out, err := exec.Command(p, "lt", "--", src).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("unrar: %s", strings.TrimSpace(string(out)))
		}
		return parseUnrarLt(string(out)), nil
	}
	return ls7z(src)
}

func parseUnrarLt(output string) []archiveLsEntry {
	var entries []archiveLsEntry
	var current archiveLsEntry
	inBlock := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		before, after, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		key, val := strings.TrimSpace(before), strings.TrimSpace(after)
		switch key {
		case "Name":
			if inBlock && current.Name != "" {
				entries = append(entries, current)
			}
			current = archiveLsEntry{Kind: "file", Name: val}
			inBlock = true
		case "Type":
			if strings.Contains(val, "Directory") {
				current.Kind = "dir"
			}
		case "Size":
			if s, err := strconv.ParseInt(val, 10, 64); err == nil {
				current.Size = &s
			}
		case "mtime":
			current.Mtime = val
		}
	}
	if inBlock && current.Name != "" {
		entries = append(entries, current)
	}
	return entries
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// archiveExt returns the canonical (lowercased) multi-part extension for known
// archive formats, e.g. ".tar.gz", ".7z", ".zip".
func archiveExt(path string) string {
	lower := strings.ToLower(path)
	for _, ext := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(lower, ext) {
			return ext
		}
	}
	for _, ext := range []string{".tgz", ".tbz2", ".txz", ".zip", ".7z", ".rar", ".tar", ".gz", ".bz2", ".xz"} {
		if strings.HasSuffix(lower, ext) {
			return ext
		}
	}
	return filepath.Ext(lower)
}

func isToolMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "executable file not found")
}

func requiredTool(ext string) string {
	switch ext {
	case ".7z", ".tar.xz", ".txz":
		return "7z"
	case ".rar":
		return "unrar"
	default:
		return "7z"
	}
}

func installCmd(mgr, ext string) string {
	tool := requiredTool(ext)
	switch mgr {
	case "apt":
		if tool == "unrar" {
			return "sudo apt install unrar"
		}
		return "sudo apt install p7zip-full"
	case "dnf":
		if tool == "unrar" {
			return "sudo dnf install unrar"
		}
		return "sudo dnf install p7zip p7zip-plugins"
	case "pacman":
		if tool == "unrar" {
			return "sudo pacman -S unrar"
		}
		return "sudo pacman -S p7zip"
	case "brew":
		if tool == "unrar" {
			return "brew install unrar"
		}
		return "brew install p7zip"
	}
	return ""
}
