package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// pluginManifest is the schema for config/plugins/<name>/plugin.json.
type pluginManifest struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`    // "icon-pack"
	Adapter string `json:"adapter"` // "vscode-icon-theme"
	Source  string `json:"source"`  // path to the cloned repo/VSIX dir, relative to plugin dir
	Theme   string `json:"theme"`   // path to the theme JSON, relative to Source (auto-detected if empty)

	// Server, when present, declares a sandboxed WASM backend for this plugin. It is
	// loaded and run by plugin_host.go; the client reaches it through the generic
	// POST /_api/v1/plugins/<id>/rpc endpoint. See serverPluginDef.
	Server *serverPluginDef `json:"server,omitempty"`
}

// iconTheme holds the resolved icon pack data.
type iconTheme struct {
	FileExtensions     map[string]string // lowercase ext → icon definition name
	FileNames          map[string]string // lowercase filename → icon definition name
	FolderNames        map[string]string // lowercase folder name → icon definition name (closed)
	FolderNamesExpanded map[string]string // lowercase folder name → icon definition name (open)
	File               string            // default file icon definition name
	Folder             string            // default closed folder icon definition name
	FolderExpanded     string            // default open folder icon definition name
	iconPaths          map[string]string // icon definition name → absolute path to SVG
}

// activeIconTheme is the first successfully-loaded icon pack, or nil.
var activeIconTheme *iconTheme

// has reports whether an icon definition name has a backing SVG on disk.
func (t *iconTheme) has(icon string) bool {
	_, ok := t.iconPaths[icon]
	return ok
}

// pick returns candidate if its SVG exists, otherwise fallback (which may also be "").
func (t *iconTheme) pick(candidate, fallback string) string {
	if candidate != "" && t.has(candidate) {
		return candidate
	}
	if fallback != "" && t.has(fallback) {
		return fallback
	}
	return ""
}

// resolveOpen returns the open/expanded variant icon name for a directory.
// Falls back through: named-open → named-closed → default-open → default-closed.
// Preferring the named-closed icon over the generic open folder keeps custom folder
// icons consistent when no specific open variant SVG exists on disk.
func (t *iconTheme) resolveOpen(name string) string {
	if t == nil {
		return ""
	}
	lower := strings.ToLower(name)
	// 1. Named open variant (e.g. folder-src-open)
	if icon, ok := t.FolderNamesExpanded[lower]; ok {
		if t.has(icon) {
			return icon
		}
	}
	// 2. Named closed variant — keeps the custom icon style when no open SVG exists
	if icon, ok := t.FolderNames[lower]; ok {
		if t.has(icon) {
			return icon
		}
	}
	// 3. Default open folder
	if t.has(t.FolderExpanded) {
		return t.FolderExpanded
	}
	// 4. Default closed folder
	return t.resolve(name, true)
}

// resolve returns the icon definition name for the given filename and kind.
// Only returns names whose SVG file actually exists on disk.
func (t *iconTheme) resolve(name string, isDir bool) string {
	if t == nil {
		return ""
	}
	lower := strings.ToLower(name)
	if isDir {
		if icon, ok := t.FolderNames[lower]; ok {
			if r := t.pick(icon, t.Folder); r != "" {
				return r
			}
		}
		return t.pick(t.Folder, "")
	}
	if icon, ok := t.FileNames[lower]; ok {
		if r := t.pick(icon, t.File); r != "" {
			return r
		}
	}
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		if icon, ok := t.FileExtensions[lower[dot+1:]]; ok {
			if r := t.pick(icon, t.File); r != "" {
				return r
			}
		}
	}
	return t.pick(t.File, "")
}

func loadPlugins() {
	dir := filepath.Join(configDir, "plugins")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pluginDir := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(filepath.Join(pluginDir, "plugin.json"))
		if err != nil {
			continue
		}
		var m pluginManifest
		if err := json.Unmarshal(data, &m); err != nil {
			log.Printf("plugins: %s: invalid plugin.json: %v", e.Name(), err)
			continue
		}
		if m.Type == "icon-pack" && m.Adapter == "vscode-icon-theme" {
			theme, err := loadVSCodeIconTheme(pluginDir, m)
			if err != nil {
				log.Printf("plugins: %s: %v", m.ID, err)
				continue
			}
			if activeIconTheme == nil {
				activeIconTheme = theme
			}
			log.Printf("plugins: loaded icon pack %q (%d ext, %d name, %d folder mappings)",
				m.ID, len(theme.FileExtensions), len(theme.FileNames), len(theme.FolderNames))
		}
	}
}

func loadVSCodeIconTheme(pluginDir string, m pluginManifest) (*iconTheme, error) {
	sourceDir := filepath.Join(pluginDir, filepath.FromSlash(m.Source))

	themeFile := ""
	if m.Theme != "" {
		themeFile = filepath.Join(sourceDir, filepath.FromSlash(m.Theme))
	} else {
		themeFile = detectVSCodeThemeFile(sourceDir)
	}
	if themeFile == "" {
		return nil, fmt.Errorf("cannot locate theme JSON (set \"theme\" in plugin.json or ensure package.json exists in source dir)")
	}

	raw, err := os.ReadFile(themeFile)
	if err != nil {
		return nil, fmt.Errorf("read theme file: %w", err)
	}

	var def struct {
		IconDefinitions map[string]struct {
			IconPath string `json:"iconPath"`
		} `json:"iconDefinitions"`
		FileExtensions      map[string]string `json:"fileExtensions"`
		FileNames           map[string]string `json:"fileNames"`
		FolderNames         map[string]string `json:"folderNames"`
		FolderNamesExpanded map[string]string `json:"folderNamesExpanded"`
		File                string            `json:"file"`
		Folder              string            `json:"folder"`
		FolderExpanded      string            `json:"folderExpanded"`
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		return nil, fmt.Errorf("parse theme JSON: %w", err)
	}

	themeDir := filepath.Dir(themeFile)
	iconPaths := make(map[string]string, len(def.IconDefinitions))
	for name, d := range def.IconDefinitions {
		if d.IconPath == "" {
			continue
		}
		abs, err := filepath.Abs(filepath.Join(themeDir, filepath.FromSlash(d.IconPath)))
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			iconPaths[name] = abs
		}
	}

	lower := func(m map[string]string) map[string]string {
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[strings.ToLower(k)] = v
		}
		return out
	}

	return &iconTheme{
		FileExtensions:      lower(def.FileExtensions),
		FileNames:           lower(def.FileNames),
		FolderNames:         lower(def.FolderNames),
		FolderNamesExpanded: lower(def.FolderNamesExpanded),
		File:                def.File,
		Folder:              def.Folder,
		FolderExpanded:      def.FolderExpanded,
		iconPaths:           iconPaths,
	}, nil
}

// detectVSCodeThemeFile reads package.json and returns the absolute path to the
// first contributes.iconThemes entry, or "" if not found.
func detectVSCodeThemeFile(sourceDir string) string {
	pkgData, err := os.ReadFile(filepath.Join(sourceDir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Contributes struct {
			IconThemes []struct {
				Path string `json:"path"`
			} `json:"iconThemes"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(pkgData, &pkg); err != nil || len(pkg.Contributes.IconThemes) == 0 {
		return ""
	}
	abs := filepath.Join(sourceDir, filepath.FromSlash(pkg.Contributes.IconThemes[0].Path))
	if _, err := os.Stat(abs); err != nil {
		return ""
	}
	return abs
}
