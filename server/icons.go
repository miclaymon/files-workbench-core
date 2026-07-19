package main

import (
	"net/http"
	"strings"
)

// handleIconsManifest returns the full icon-resolution tables so the client can
// resolve icon names locally without a round-trip per file.
func handleIconsManifest(w http.ResponseWriter, r *http.Request) {
	if activeIconTheme == nil {
		jsonOK(w, map[string]any{"available": false})
		return
	}
	t := activeIconTheme
	jsonOK(w, map[string]any{
		"available":           true,
		"fileExtensions":      t.FileExtensions,
		"fileNames":           t.FileNames,
		"folderNames":         t.FolderNames,
		"folderNamesExpanded": t.FolderNamesExpanded,
		"file":                t.File,
		"folder":              t.Folder,
		"folderExpanded":      t.FolderExpanded,
	})
}

// handleIconsSvg serves a single SVG icon file by definition name.
// Query param: name=<icon definition name> (e.g. "_ts", "_folder")
// The .svg suffix is stripped if present for convenience.
func handleIconsSvg(w http.ResponseWriter, r *http.Request) {
	if activeIconTheme == nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSuffix(r.URL.Query().Get("name"), ".svg")
	if name == "" {
		jsonErr(w, http.StatusBadRequest, "name required")
		return
	}
	path, ok := activeIconTheme.iconPaths[name]
	if !ok {
		// Respond with image/svg+xml even on 404 — keeps Content-Type as an image so
		// cross-origin <img> requests aren't blocked by ORB, while still firing @error.
		w.Header().Set("Content-Type", "image/svg+xml")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, path)
}
