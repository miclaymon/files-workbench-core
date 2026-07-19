package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ── Serving runtime plugin artifacts to the client ────────────────────────────
//
// Client plugins are loaded at runtime (not bundled): the client fetches the plugin
// manifest, verifies each artifact's content hash, then dynamic-imports its client.js.
// This file serves both. Two directories are scanned:
//   pluginsDistDir       — first-party built artifacts (FW_PLUGINS_DIR, or repo .fw/plugins
//                          in dev; read-only app resources when installed).
//   thirdPartyPluginsDir — user-installed plugins (dataDir/plugins; writable).
// Both are resolved in main().

var (
	pluginsDistDir       string
	thirdPartyPluginsDir string
	pluginRegistryURL    string // optional remote plugin index (FW_PLUGIN_REGISTRY)
)

type servedArtifact struct {
	URL  string `json:"url"`
	Hash string `json:"hash"`
}

type servedPlugin struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Icon        string          `json:"icon,omitempty"`
	Permissions []string        `json:"permissions"`
	Client      *servedArtifact `json:"client,omitempty"`
	FirstParty  bool            `json:"firstParty"`
	Enabled     bool            `json:"enabled"`
}

// allowedArtifacts is the whitelist of files the artifact endpoint will serve — no
// arbitrary path access into a plugin dir.
var allowedArtifacts = map[string]bool{
	"client.js":   true,
	"plugin.json": true,
	"server.wasm": true,
}

// safeSegment rejects empty, separator-bearing, or dot-dot path segments.
func safeSegment(s string) bool {
	return s != "" && s != "." && s != ".." &&
		!strings.ContainsAny(s, "/\\") && !strings.Contains(s, "..")
}

// scanServedPlugins reads <dir>/<id>/plugin.json (the runtime manifest emitted by the
// client build) into servedPlugin entries.
func scanServedPlugins(dir string, firstParty bool) []servedPlugin {
	out := []servedPlugin{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || !safeSegment(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "plugin.json"))
		if err != nil {
			continue
		}
		var m struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Version     string   `json:"version"`
			Icon        string   `json:"icon"`
			Permissions []string `json:"permissions"`
			Client      *struct {
				Entry string `json:"entry"`
				Hash  string `json:"hash"`
			} `json:"client"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		// Only runtime CLIENT plugins are surfaced here — the client loader consumes
		// client artifacts. (Icon-pack / server-only manifests in config/plugins, which
		// dev's data dir aliases, have no client target and are skipped.)
		if m.Client == nil || m.Client.Entry == "" {
			continue
		}
		id := m.ID
		if id == "" {
			id = e.Name()
		}
		out = append(out, servedPlugin{
			ID: id, Name: m.Name, Version: m.Version, Icon: m.Icon, Permissions: m.Permissions,
			FirstParty: firstParty, Enabled: true,
			Client:     &servedArtifact{URL: apiPrefix + "/plugins/" + id + "/" + m.Client.Entry, Hash: m.Client.Hash},
		})
	}
	return out
}

// handlePluginsManifest lists all runtime-loadable plugins (first-party + third-party),
// stamping each with its enabled state (disabled plugins are listed but not auto-loaded).
func handlePluginsManifest(w http.ResponseWriter, r *http.Request) {
	out := scanServedPlugins(pluginsDistDir, true)
	out = append(out, scanServedPlugins(thirdPartyPluginsDir, false)...)
	st := readPluginState()
	for i := range out {
		if st.Disabled[out[i].ID] {
			out[i].Enabled = false
		}
	}
	jsonOK(w, map[string]any{"plugins": out})
}

// handlePluginArtifact serves a whitelisted artifact from a plugin dir (first-party
// dir wins), with path-traversal guards.
func handlePluginArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	artifact := r.PathValue("artifact")
	if !safeSegment(id) || !allowedArtifacts[artifact] {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	for _, base := range []string{pluginsDistDir, thirdPartyPluginsDir} {
		if base == "" {
			continue
		}
		p := filepath.Join(base, id, artifact)
		// Confirm the resolved path stays within base/<id> (defense in depth).
		if rel, err := filepath.Rel(filepath.Join(base, id), p); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			w.Header().Set("Cache-Control", "no-cache") // hashes change per build; client verifies
			http.ServeFile(w, r, p)
			return
		}
	}
	jsonErr(w, http.StatusNotFound, "artifact not found")
}
