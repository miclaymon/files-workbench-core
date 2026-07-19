package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// appCustomSection is the group under which this app stores its own (non-freedesktop)
// directory-customization keys inside a .directory file. The "X-" prefix follows the
// freedesktop Desktop Entry convention for vendor extensions, so other tools ignore it.
const appCustomSection = "X-Files-Workbench"

// dirCustomization holds metadata parsed from a directory customization file
// (.directory, desktop.ini, or .DS_Store).
type dirCustomization struct {
	Source  string `json:"source"`             // ".directory", "desktop.ini", ".DS_Store"
	Name    string `json:"name,omitempty"`     // custom display name
	Icon    string `json:"icon,omitempty"`     // icon resolved for display (abs path for relative / ~ icons)
	IconRaw string `json:"icon_raw,omitempty"` // icon exactly as written in the file
	Comment string `json:"comment,omitempty"`  // tooltip or description
}

// readDirCustomization looks for customization files inside dirPath and returns merged
// metadata (with its icon resolved for display). Returns nil if none is found.
//
// Both .directory and desktop.ini are read and merged by field, so a value one file
// defines wins even when the other file exists. For each field the app group
// ([X-Files-Workbench]) takes priority over the standard group, and within a tier
// .directory (our canonical file) beats desktop.ini. For icons that gives the order the
// UI resolver expects:
//
//	1a  .directory   [X-Files-Workbench] Icon
//	1b  desktop.ini   [X-Files-Workbench] Icon
//	2a  .directory   [Desktop Entry]     Icon   (e.g. "folder-violet", a path, a theme name)
//	2b  desktop.ini   [.ShellClassInfo]   IconResource / IconFile
//
// (Whatever wins then flows through the client's custom-icon → icon-pack → default
// chain.) This is an internal server read — it intentionally bypasses the blacklist,
// which only governs what appears in API listings, not what the server reads for its
// own computation (enriching a directory with its custom icon / display name).
func readDirCustomization(dirPath string) *dirCustomization {
	var dot, ini map[string]map[string]string
	if data, err := os.ReadFile(filepath.Join(dirPath, ".directory")); err == nil {
		dot = parseINI(data)
	}
	iniIsCustom := false
	if data, err := os.ReadFile(filepath.Join(dirPath, "desktop.ini")); err == nil {
		ini = parseINI(data)
		// A generic desktop.ini (config/marker file) with neither a shell-class nor an
		// app group isn't a directory customization — ignore its contents.
		_, hasShell := ini[".ShellClassInfo"]
		_, hasApp := ini[appCustomSection]
		iniIsCustom = hasShell || hasApp
	}
	if !iniIsCustom {
		ini = nil
	}

	if dot == nil && ini == nil {
		// .DS_Store — macOS (binary format; presence is all we detect)
		if _, err := os.Stat(filepath.Join(dirPath, ".DS_Store")); err == nil {
			return &dirCustomization{Source: ".DS_Store"}
		}
		return nil
	}

	c := &dirCustomization{Source: ".directory"}
	if dot == nil {
		c.Source = "desktop.ini"
	}

	rawIcon := firstNonEmpty(
		iniGet(dot, appCustomSection, "Icon"), // 1a
		iniGet(ini, appCustomSection, "Icon"), // 1b
		iniGet(dot, "Desktop Entry", "Icon"),  // 2a
		desktopIniIcon(ini),                   // 2b
	)
	c.Name = firstNonEmpty(
		iniGet(dot, appCustomSection, "Name"),
		iniGet(ini, appCustomSection, "Name"),
		iniGet(dot, "Desktop Entry", "Name"),
	)
	c.Comment = firstNonEmpty(
		iniGet(dot, appCustomSection, "Comment"),
		iniGet(ini, appCustomSection, "Comment"),
		iniGet(dot, "Desktop Entry", "Comment"),
		iniGet(ini, ".ShellClassInfo", "InfoTip"),
	)

	if rawIcon != "" {
		// Split into the raw value (as written) and a display value: absolute and
		// relative *file* icons become absolute paths the client loads via /fs/preview,
		// while folder-<color> names and XDG theme names (no matching file) pass through
		// for the client to resolve.
		c.IconRaw = rawIcon
		c.Icon = resolveIconValue(dirPath, rawIcon)
	}

	// A desktop.ini-only source that yielded nothing usable isn't a real customization.
	if dot == nil && c.Name == "" && c.Icon == "" && c.Comment == "" {
		return nil
	}
	return c
}

func iniGet(secs map[string]map[string]string, section, key string) string {
	if secs == nil {
		return ""
	}
	if s := secs[section]; s != nil {
		return s[key]
	}
	return ""
}

// desktopIniIcon returns the Windows shell folder icon: IconResource ("path,index")
// takes priority over IconFile.
func desktopIniIcon(ini map[string]map[string]string) string {
	if v := iniGet(ini, ".ShellClassInfo", "IconResource"); v != "" {
		return v
	}
	return iniGet(ini, ".ShellClassInfo", "IconFile")
}

func resolveIconValue(dirPath, icon string) string {
	switch {
	case icon == "":
		return ""
	case strings.HasPrefix(icon, "/"):
		return icon
	case strings.HasPrefix(icon, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, icon[2:])
		}
		return icon
	}
	// Relative path to an image inside the directory (e.g. "cover.png", "art/folder.jpg").
	// Only treat it as a file icon if it actually exists — otherwise it's a folder-<color>
	// name or an XDG icon-theme name that the client resolves on its own.
	cand := filepath.Join(dirPath, icon)
	if info, err := os.Stat(cand); err == nil && !info.IsDir() {
		return cand
	}
	return icon
}

// ── INI parsing (flat) ──────────────────────────────────────────────────────────
//
// parseINI parses INI-style content into section → key → value.
// - BOM is stripped automatically.
// - Comment lines starting with ; or # are skipped.
// - For duplicate keys within a section, the first occurrence wins.
// - Localized keys (e.g. "Name[nl]") are stored verbatim.
func parseINI(data []byte) map[string]map[string]string {
	out := make(map[string]map[string]string)
	section := ""
	// Strip UTF-8 BOM (common in Windows-generated INI files)
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if out[section] == nil {
				out[section] = make(map[string]string)
			}
			continue
		}
		if i := strings.IndexByte(line, '='); i >= 0 {
			key := strings.TrimSpace(line[:i])
			val := strings.TrimSpace(line[i+1:])
			if out[section] == nil {
				out[section] = make(map[string]string)
			}
			if _, exists := out[section][key]; !exists {
				out[section][key] = val
			}
		}
	}
	return out
}

func parseDesktopIni(data []byte) *dirCustomization {
	sections := parseINI(data)
	// Windows folder customization lives in [.ShellClassInfo].
	// Generic desktop.ini files (config, readme markers, etc.) that lack this
	// section are not directory customizations and should be ignored.
	shell := sections[".ShellClassInfo"]
	if shell == nil {
		return nil
	}
	c := &dirCustomization{Source: "desktop.ini"}
	// IconResource ("path,index") takes priority over IconFile
	if v := shell["IconResource"]; v != "" {
		c.Icon = v
	} else {
		c.Icon = shell["IconFile"]
	}
	c.Comment = shell["InfoTip"]
	return c
}

// ── INI document (order-preserving, editable) ─────────────────────────────────────
//
// iniDoc keeps every source line of a .directory file — section headers, key=value
// pairs, comments, and blanks — so an edit can change or remove a single key while
// leaving the rest of the file (comments, ordering, unknown groups, localized keys)
// intact. This is what makes writes lossless, unlike a parse-then-reserialize.

type iniLine struct {
	kind    string // "section" | "kv" | "raw" (comment or blank)
	section string // section this line belongs to ("" = preamble before the first header)
	key     string // kv only
	value   string // kv only
	text    string // verbatim source text (used for "section"/"raw"; kv is rebuilt from key/value)
}

type iniDoc struct {
	lines []iniLine
	crlf  bool
}

func parseIniDoc(data []byte) *iniDoc {
	d := &iniDoc{}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	d.crlf = bytes.Contains(data, []byte("\r\n"))
	norm := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))

	section := ""
	sc := bufio.NewScanner(bytes.NewReader(norm))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"):
			section = trimmed[1 : len(trimmed)-1]
			d.lines = append(d.lines, iniLine{kind: "section", section: section, text: line})
		case trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#"):
			d.lines = append(d.lines, iniLine{kind: "raw", section: section, text: line})
		default:
			if i := strings.IndexByte(line, '='); i >= 0 {
				d.lines = append(d.lines, iniLine{
					kind:    "kv",
					section: section,
					key:     strings.TrimSpace(line[:i]),
					value:   strings.TrimSpace(line[i+1:]),
				})
			} else {
				d.lines = append(d.lines, iniLine{kind: "raw", section: section, text: line})
			}
		}
	}
	return d
}

func (d *iniDoc) get(section, key string) (string, bool) {
	for _, l := range d.lines {
		if l.kind == "kv" && l.section == section && l.key == key {
			return l.value, true
		}
	}
	return "", false
}

// sectionsMap returns a flat section → key → value view (first key wins), for reads.
func (d *iniDoc) sectionsMap() map[string]map[string]string {
	out := make(map[string]map[string]string)
	for _, l := range d.lines {
		if l.kind != "kv" {
			continue
		}
		if out[l.section] == nil {
			out[l.section] = make(map[string]string)
		}
		if _, ok := out[l.section][l.key]; !ok {
			out[l.section][l.key] = l.value
		}
	}
	return out
}

// set updates an existing key in place, else appends it after the last key of its
// section — creating the section (preceded by a blank line) if it doesn't exist.
func (d *iniDoc) set(section, key, value string) {
	for i := range d.lines {
		if d.lines[i].kind == "kv" && d.lines[i].section == section && d.lines[i].key == key {
			d.lines[i].value = value
			return
		}
	}
	newLine := iniLine{kind: "kv", section: section, key: key, value: value}

	lastContentIdx := -1
	sectionExists := false
	for i, l := range d.lines {
		if l.section != section {
			continue
		}
		if l.kind == "section" {
			sectionExists = true
		}
		if l.kind == "section" || l.kind == "kv" {
			lastContentIdx = i
		}
	}

	if sectionExists {
		at := lastContentIdx + 1
		d.lines = append(d.lines[:at], append([]iniLine{newLine}, d.lines[at:]...)...)
		return
	}
	// Section missing: separate from prior content with a blank line, then add it.
	if n := len(d.lines); n > 0 && strings.TrimSpace(d.lines[n-1].text) != "" {
		d.lines = append(d.lines, iniLine{kind: "raw", text: ""})
	}
	d.lines = append(d.lines,
		iniLine{kind: "section", section: section, text: "[" + section + "]"},
		newLine,
	)
}

// del removes a single key line. Returns whether a line was removed.
func (d *iniDoc) del(section, key string) bool {
	for i, l := range d.lines {
		if l.kind == "kv" && l.section == section && l.key == key {
			d.lines = append(d.lines[:i], d.lines[i+1:]...)
			return true
		}
	}
	return false
}

func (d *iniDoc) bytes() []byte {
	nl := "\n"
	if d.crlf {
		nl = "\r\n"
	}
	var sb strings.Builder
	for _, l := range d.lines {
		if l.kind == "kv" {
			sb.WriteString(l.key + "=" + l.value)
		} else {
			sb.WriteString(l.text)
		}
		sb.WriteString(nl)
	}
	return []byte(sb.String())
}

// loadOrSeedDirectoryDoc loads a directory's .directory as an editable doc. When none
// exists it starts a fresh [Desktop Entry] doc, importing any desktop.ini customization
// (name/icon/comment) so noteworthy values carry over to the new .directory.
func loadOrSeedDirectoryDoc(dirPath string) *iniDoc {
	if data, err := os.ReadFile(filepath.Join(dirPath, ".directory")); err == nil {
		return parseIniDoc(data)
	}
	d := &iniDoc{}
	d.lines = append(d.lines,
		iniLine{kind: "section", section: "Desktop Entry", text: "[Desktop Entry]"},
		iniLine{kind: "kv", section: "Desktop Entry", key: "Type", value: "Directory"},
	)
	if data, err := os.ReadFile(filepath.Join(dirPath, "desktop.ini")); err == nil {
		if c := parseDesktopIni(data); c != nil {
			if c.Name != "" {
				d.set("Desktop Entry", "Name", c.Name)
			}
			if c.Icon != "" {
				d.set("Desktop Entry", "Icon", c.Icon) // raw desktop.ini value
			}
			if c.Comment != "" {
				d.set("Desktop Entry", "Comment", c.Comment)
			}
		}
	}
	return d
}

func writeDirectoryDoc(dirPath string, doc *iniDoc) error {
	return os.WriteFile(filepath.Join(dirPath, ".directory"), doc.bytes(), 0644)
}

// customizationPayload is the shared GET/PUT/PATCH response: the resolved typed summary
// plus the raw, editable groups from the .directory file.
func customizationPayload(dirPath string, extra map[string]any) map[string]any {
	sections := make(map[string]map[string]string)
	if data, err := os.ReadFile(filepath.Join(dirPath, ".directory")); err == nil {
		sections = parseIniDoc(data).sectionsMap()
	}
	out := map[string]any{
		"customization": readDirCustomization(dirPath),
		"writableFile":  ".directory",
		"sections":      sections,
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

// handleFsCustomizationGet returns the parsed customization for a single directory,
// plus the raw editable groups from its .directory file.
//
//	GET /_api/v1/fs/customization?path=<dir>
func handleFsCustomizationGet(w http.ResponseWriter, r *http.Request) {
	dirPath := r.URL.Query().Get("path")
	if dirPath == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	jsonOK(w, customizationPayload(dirPath, nil))
}

// handleFsCustomizationPut writes or updates the common typed fields of a directory's
// .directory file, losslessly (other keys/sections/comments are preserved). Fields set
// to null in the body are left unchanged; fields set to "" are removed.
//
//	PUT /_api/v1/fs/customization?path=<dir>
//	Body: { "name": "...", "icon": "...", "comment": "..." }
func handleFsCustomizationPut(w http.ResponseWriter, r *http.Request) {
	dirPath := r.URL.Query().Get("path")
	if dirPath == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	info, err := os.Stat(dirPath)
	if err != nil || !info.IsDir() {
		jsonErr(w, http.StatusBadRequest, "path is not a directory")
		return
	}

	// Pointer fields distinguish "omitted" (nil = keep) from "set to empty" (ptr to "" = remove).
	var body struct {
		Name    *string `json:"name"`
		Icon    *string `json:"icon"`
		Comment *string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	doc := loadOrSeedDirectoryDoc(dirPath)
	applyTypedField(doc, "Name", body.Name)
	applyTypedField(doc, "Icon", body.Icon)
	applyTypedField(doc, "Comment", body.Comment)

	if err := writeDirectoryDoc(dirPath, doc); err != nil {
		jsonErr(w, http.StatusInternalServerError, "write failed: "+err.Error())
		return
	}
	jsonOK(w, customizationPayload(dirPath, map[string]any{"ok": true}))
}

// ── pinned items ──────────────────────────────────────────────────────────────
//
// Pinned item names live in a directory's .directory under [X-Files-Workbench] as a
// freedesktop string list: ';'-separated, ';'-terminated, with '\;' / '\\' escaping so
// filenames containing ';' or '\' round-trip. Directory listings group pinned items
// first (see simpleListDir).

func parsePinnedList(s string) []string {
	var out []string
	var cur strings.Builder
	esc, started := false, false
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
			started = true
		case r == ';':
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush()
	return out
}

func formatPinnedList(names []string) string {
	var sb strings.Builder
	for _, n := range names {
		if n == "" {
			continue
		}
		e := strings.ReplaceAll(n, "\\", "\\\\")
		e = strings.ReplaceAll(e, ";", "\\;")
		sb.WriteString(e)
		sb.WriteByte(';')
	}
	return sb.String()
}

// readPinnedNames returns the set of pinned item names for a directory (nil if none).
func readPinnedNames(dirPath string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(dirPath, ".directory"))
	if err != nil {
		return nil
	}
	raw := iniGet(parseINI(data), appCustomSection, "Pinned")
	if raw == "" {
		return nil
	}
	set := make(map[string]bool)
	for _, n := range parsePinnedList(raw) {
		set[n] = true
	}
	return set
}

// handleFsPin adds or removes item names from a directory's pinned list, losslessly.
//
//	POST /_api/v1/fs/pin
//	Body: { "path": "<dir>", "names": ["a.txt","sub"], "pinned": true|false }
func handleFsPin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path   string   `json:"path"`
		Names  []string `json:"names"`
		Pinned bool     `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	info, err := os.Stat(body.Path)
	if err != nil || !info.IsDir() {
		jsonErr(w, http.StatusBadRequest, "path is not a directory")
		return
	}

	doc := loadOrSeedDirectoryDoc(body.Path)
	cur, _ := doc.get(appCustomSection, "Pinned")

	target := make(map[string]bool, len(body.Names))
	for _, n := range body.Names {
		if n != "" {
			target[n] = true
		}
	}
	// Keep existing order, dedupe, drop the targets on unpin.
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, n := range parsePinnedList(cur) {
		if seen[n] || (target[n] && !body.Pinned) {
			continue
		}
		seen[n] = true
		result = append(result, n)
	}
	// Append newly-pinned names not already present.
	if body.Pinned {
		for _, n := range body.Names {
			if n != "" && !seen[n] {
				seen[n] = true
				result = append(result, n)
			}
		}
	}

	if len(result) == 0 {
		doc.del(appCustomSection, "Pinned")
	} else {
		doc.set(appCustomSection, "Pinned", formatPinnedList(result))
	}
	if err := writeDirectoryDoc(body.Path, doc); err != nil {
		jsonErr(w, http.StatusInternalServerError, "write failed: "+err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "pinned": result})
}

// applyTypedField maps a nullable typed field onto [Desktop Entry]: nil keeps the key,
// "" removes it, any other value sets it.
func applyTypedField(doc *iniDoc, key string, val *string) {
	if val == nil {
		return
	}
	if *val == "" {
		doc.del("Desktop Entry", key)
		return
	}
	doc.set("Desktop Entry", key, *val)
}

// handleFsCustomizationPatch applies generic set/delete operations to arbitrary
// keys of a directory's .directory file, losslessly. Ops with no section default to
// the app group ([X-Files-Workbench]).
//
//	PATCH /_api/v1/fs/customization?path=<dir>
//	Body: { "ops": [ { "op": "set"|"delete", "section": "...", "key": "...", "value": "..." } ] }
func handleFsCustomizationPatch(w http.ResponseWriter, r *http.Request) {
	dirPath := r.URL.Query().Get("path")
	if dirPath == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	info, err := os.Stat(dirPath)
	if err != nil || !info.IsDir() {
		jsonErr(w, http.StatusBadRequest, "path is not a directory")
		return
	}

	var body struct {
		Ops []struct {
			Op      string `json:"op"`
			Section string `json:"section"`
			Key     string `json:"key"`
			Value   string `json:"value"`
		} `json:"ops"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	doc := loadOrSeedDirectoryDoc(dirPath)
	for _, op := range body.Ops {
		section := op.Section
		if section == "" {
			section = appCustomSection
		}
		if op.Key == "" {
			jsonErr(w, http.StatusBadRequest, "each op requires a key")
			return
		}
		switch op.Op {
		case "set":
			doc.set(section, op.Key, op.Value)
		case "delete", "del", "remove":
			doc.del(section, op.Key)
		default:
			jsonErr(w, http.StatusBadRequest, "unknown op: "+op.Op)
			return
		}
	}

	if err := writeDirectoryDoc(dirPath, doc); err != nil {
		jsonErr(w, http.StatusInternalServerError, "write failed: "+err.Error())
		return
	}
	jsonOK(w, customizationPayload(dirPath, map[string]any{"ok": true}))
}
