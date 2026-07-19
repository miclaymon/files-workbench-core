package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// pluginIDRe matches a kebab-case plugin id (mirrors the client validator).
var pluginIDRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ── Third-party plugin install / uninstall / enable-disable ───────────────────
//
// User-installed plugins live in thirdPartyPluginsDir (dataDir/plugins, writable).
// A package is a .zip/.vsix of a BUILT plugin dir — the contents of what the client
// build emits into .fw/plugins/<id>/: client.js + plugin.json (the runtime manifest),
// optionally server.wasm. We re-derive the client hash server-side so the served hash
// always equals the file on disk (trust-on-first-use integrity — the package can't
// under-report a tampered artifact). The runtime loader still hash-verifies on load.
//
// Only whitelisted artifacts are copied into place (client.js / server.wasm) plus a
// regenerated plugin.json, so a package can't smuggle extra served files.

const maxPluginPackageBytes = 64 << 20 // 64 MiB cap on an uploaded/downloaded package

// installManifest is the loose shape we read from a package's plugin.json (preferred)
// or manifest.json. `client.entry` is the built entry file (e.g. "client.js").
type installManifest struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Icon        string   `json:"icon"`
	Permissions []string `json:"permissions"`
	Client      *struct {
		Entry string `json:"entry"`
		Hash  string `json:"hash"`
	} `json:"client"`
	Server json.RawMessage `json:"server"`
}

// ── enable/disable state (dataDir/plugins/state.json) ─────────────────────────

type pluginStateFile struct {
	Disabled map[string]bool `json:"disabled"` // id → true = disabled (not auto-loaded)
}

func pluginStatePath() string { return filepath.Join(thirdPartyPluginsDir, "state.json") }

func readPluginState() pluginStateFile {
	st := pluginStateFile{Disabled: map[string]bool{}}
	data, err := os.ReadFile(pluginStatePath())
	if err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.Disabled == nil {
		st.Disabled = map[string]bool{}
	}
	return st
}

func writePluginState(st pluginStateFile) error {
	if err := os.MkdirAll(thirdPartyPluginsDir, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(pluginStatePath(), append(data, '\n'), 0o644)
}

// isFirstPartyPlugin reports whether id is a bundled first-party plugin (present in the
// read-only dist dir) — those can't be uninstalled or overwritten by an install.
func isFirstPartyPlugin(id string) bool {
	if pluginsDistDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(pluginsDistDir, id, "plugin.json"))
	return err == nil
}

// ── install core ──────────────────────────────────────────────────────────────

// extractPluginZip unpacks a zip/vsix into destDir with zip-slip + size guards.
func extractPluginZip(zipPath, destDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("not a valid zip/vsix: %w", err)
	}
	defer zr.Close()

	var total int64
	for _, f := range zr.File {
		// Reject absolute/dot-dot paths; keep everything inside destDir.
		clean := filepath.Clean(f.Name)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe path in package: %s", f.Name)
		}
		target := filepath.Join(destDir, clean)
		if rel, err := filepath.Rel(destDir, target); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("unsafe path in package: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.CopyN(out, rc, maxPluginPackageBytes-total+1)
		rc.Close()
		out.Close()
		total += n
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if total > maxPluginPackageBytes {
			return errors.New("package too large")
		}
	}
	return nil
}

// locatePluginRoot finds the dir holding plugin.json (or manifest.json): the extract
// root, or its single top-level subdir (packages often wrap everything in one folder).
func locatePluginRoot(base string) (string, error) {
	for _, name := range []string{"plugin.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(base, name)); err == nil {
			return base, nil
		}
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 1 {
		sub := filepath.Join(base, dirs[0])
		for _, name := range []string{"plugin.json", "manifest.json"} {
			if _, err := os.Stat(filepath.Join(sub, name)); err == nil {
				return sub, nil
			}
		}
	}
	return "", errors.New("package has no plugin.json or manifest.json at its root")
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// installFromDir validates an extracted plugin dir and moves its whitelisted artifacts
// into thirdPartyPluginsDir/<id>/, writing a regenerated plugin.json with a server-
// recomputed client hash. Reused by both the file-upload and (Phase B) URL install.
func installFromDir(extractRoot string, force bool) (installManifest, error) {
	var mf installManifest
	root, err := locatePluginRoot(extractRoot)
	if err != nil {
		return mf, err
	}

	// Read plugin.json if present, else manifest.json.
	var data []byte
	for _, name := range []string{"plugin.json", "manifest.json"} {
		if d, err := os.ReadFile(filepath.Join(root, name)); err == nil {
			data = d
			break
		}
	}
	if err := json.Unmarshal(data, &mf); err != nil {
		return mf, fmt.Errorf("invalid manifest: %w", err)
	}

	if !safeSegment(mf.ID) || !pluginIDRe.MatchString(mf.ID) {
		return mf, errors.New("manifest id must be kebab-case (a-z, 0-9, hyphen)")
	}
	if mf.Client == nil || !safeSegment(mf.Client.Entry) {
		return mf, errors.New("manifest needs a client target with a flat built entry (e.g. client.js)")
	}
	entryPath := filepath.Join(root, mf.Client.Entry)
	if info, err := os.Stat(entryPath); err != nil || info.IsDir() {
		return mf, fmt.Errorf("client entry %q not found in package (build with `npm run build:plugins` and zip the output dir)", mf.Client.Entry)
	}
	if isFirstPartyPlugin(mf.ID) {
		return mf, fmt.Errorf("%q is a built-in plugin and cannot be replaced", mf.ID)
	}

	dest := filepath.Join(thirdPartyPluginsDir, mf.ID)
	if _, err := os.Stat(dest); err == nil && !force {
		return mf, fmt.Errorf("plugin %q is already installed (reinstall with force)", mf.ID)
	}

	// Recompute the client hash server-side (don't trust the package's claim).
	hash, err := sha256File(entryPath)
	if err != nil {
		return mf, err
	}
	mf.Client.Hash = hash

	// Stage into a temp sibling, then swap in atomically-ish.
	staging := dest + ".installing"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return mf, err
	}
	// Copy only whitelisted artifacts (client entry + optional server.wasm).
	if err := copyFile(entryPath, filepath.Join(staging, mf.Client.Entry), 0o644); err != nil {
		os.RemoveAll(staging)
		return mf, err
	}
	if _, err := os.Stat(filepath.Join(root, "server.wasm")); err == nil {
		if err := copyFile(filepath.Join(root, "server.wasm"), filepath.Join(staging, "server.wasm"), 0o644); err != nil {
			os.RemoveAll(staging)
			return mf, err
		}
	}
	// Write the regenerated runtime manifest.
	runtime := map[string]any{
		"id": mf.ID, "name": mf.Name, "version": mf.Version, "icon": mf.Icon,
		"permissions": mf.Permissions,
		"client":      map[string]string{"entry": mf.Client.Entry, "hash": hash},
	}
	if len(mf.Server) > 0 {
		runtime["server"] = mf.Server
	}
	out, _ := json.MarshalIndent(runtime, "", "  ")
	if err := os.WriteFile(filepath.Join(staging, "plugin.json"), append(out, '\n'), 0o644); err != nil {
		os.RemoveAll(staging)
		return mf, err
	}

	// Swap staging → dest.
	_ = os.RemoveAll(dest)
	if err := os.Rename(staging, dest); err != nil {
		os.RemoveAll(staging)
		return mf, err
	}

	// A reinstall re-enables the plugin.
	st := readPluginState()
	if st.Disabled[mf.ID] {
		delete(st.Disabled, mf.ID)
		_ = writePluginState(st)
	}
	return mf, nil
}

// ── HTTP handlers (control server, 8002) ──────────────────────────────────────

// handlePluginInstall installs from an uploaded .zip/.vsix (multipart field "package")
// or, with a JSON body { url, hash }, from a remote package the server downloads and
// verifies against the registry-declared hash.
func handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	ct := r.Header.Get("Content-Type")

	var mf installManifest
	var err error
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err = r.ParseMultipartForm(maxPluginPackageBytes); err != nil {
			jsonErr(w, http.StatusBadRequest, "could not read upload: "+err.Error())
			return
		}
		file, _, ferr := r.FormFile("package")
		if ferr != nil {
			jsonErr(w, http.StatusBadRequest, "missing 'package' file field")
			return
		}
		defer file.Close()
		mf, err = installUploadedPackage(file, force)
	case strings.HasPrefix(ct, "application/json"):
		var body struct {
			URL  string `json:"url"`
			Hash string `json:"hash"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.URL == "" {
			jsonErr(w, http.StatusBadRequest, "expected { url, hash }")
			return
		}
		mf, err = installFromURL(body.URL, body.Hash, force)
	default:
		jsonErr(w, http.StatusBadRequest, "expected a multipart upload or a JSON { url, hash } body")
		return
	}
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOK(w, map[string]any{"plugin": servedForInstalled(mf)})
}

// installFromURL downloads a package (http/https only), verifies it against the
// expected sha256 (from the registry index), then installs it.
func installFromURL(url, expectHash string, force bool) (installManifest, error) {
	var mf installManifest
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return mf, errors.New("package url must be http(s)")
	}
	resp, err := http.Get(url)
	if err != nil {
		return mf, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return mf, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	tmp, err := os.MkdirTemp("", "fw-plugin-dl-")
	if err != nil {
		return mf, err
	}
	defer os.RemoveAll(tmp)
	zipPath := filepath.Join(tmp, "package.zip")
	out, err := os.Create(zipPath)
	if err != nil {
		return mf, err
	}
	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(out, h), resp.Body, maxPluginPackageBytes+1)
	out.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return mf, err
	}
	if n > maxPluginPackageBytes {
		return mf, errors.New("package too large")
	}
	if expectHash != "" {
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, expectHash) {
			return mf, fmt.Errorf("package hash mismatch (want %s…, got %s…)", trunc(expectHash), trunc(got))
		}
	}

	extractDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return mf, err
	}
	if err := extractPluginZip(zipPath, extractDir); err != nil {
		return mf, err
	}
	return installFromDir(extractDir, force)
}

func trunc(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ── Remote registry (data server) ─────────────────────────────────────────────

var registryCache struct {
	at   int64
	body []byte
}

const registryTTLSeconds = 60

// handlePluginRegistry proxies the configured registry index (FW_PLUGIN_REGISTRY), with
// a short TTL cache. Returns { plugins: [] } when no registry is configured.
func handlePluginRegistry(w http.ResponseWriter, r *http.Request) {
	if pluginRegistryURL == "" {
		jsonOK(w, map[string]any{"plugins": []any{}, "configured": false})
		return
	}
	now := time.Now().Unix()
	if registryCache.body != nil && now-registryCache.at < registryTTLSeconds {
		w.Header().Set("Content-Type", "application/json")
		w.Write(registryCache.body)
		return
	}
	resp, err := http.Get(pluginRegistryURL)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "registry fetch failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		jsonErr(w, http.StatusBadGateway, fmt.Sprintf("registry fetch failed: HTTP %d", resp.StatusCode))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPluginPackageBytes))
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "registry read failed: "+err.Error())
		return
	}
	// Validate it's JSON with a plugins array before caching/returning.
	var probe struct {
		Plugins []json.RawMessage `json:"plugins"`
	}
	if json.Unmarshal(body, &probe) != nil {
		jsonErr(w, http.StatusBadGateway, "registry index is not valid JSON { plugins: [...] }")
		return
	}
	registryCache.at = now
	registryCache.body = body
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// installUploadedPackage streams an uploaded package to a temp file, extracts it, and
// installs it. Reused shape for Phase B's URL download.
func installUploadedPackage(src io.Reader, force bool) (installManifest, error) {
	var mf installManifest
	tmp, err := os.MkdirTemp("", "fw-plugin-install-")
	if err != nil {
		return mf, err
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, "package.zip")
	out, err := os.Create(zipPath)
	if err != nil {
		return mf, err
	}
	if _, err := io.CopyN(out, src, maxPluginPackageBytes+1); err != nil && !errors.Is(err, io.EOF) {
		out.Close()
		return mf, err
	}
	out.Close()
	if info, _ := os.Stat(zipPath); info != nil && info.Size() > maxPluginPackageBytes {
		return mf, errors.New("package too large")
	}

	extractDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return mf, err
	}
	if err := extractPluginZip(zipPath, extractDir); err != nil {
		return mf, err
	}
	return installFromDir(extractDir, force)
}

// handlePluginUninstall removes a third-party plugin (refuses first-party).
func handlePluginUninstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeSegment(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	if isFirstPartyPlugin(id) {
		jsonErr(w, http.StatusForbidden, "built-in plugins cannot be uninstalled")
		return
	}
	dest := filepath.Join(thirdPartyPluginsDir, id)
	if _, err := os.Stat(dest); err != nil {
		jsonErr(w, http.StatusNotFound, "plugin not installed")
		return
	}
	if err := os.RemoveAll(dest); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := readPluginState()
	if _, ok := st.Disabled[id]; ok {
		delete(st.Disabled, id)
		_ = writePluginState(st)
	}
	jsonOK(w, map[string]any{"ok": true})
}

// handlePluginSetEnabled toggles a plugin's enabled state ({ "enabled": bool }).
func handlePluginSetEnabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeSegment(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	st := readPluginState()
	if body.Enabled {
		delete(st.Disabled, id)
	} else {
		st.Disabled[id] = true
	}
	if err := writePluginState(st); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "enabled": body.Enabled})
}

// servedForInstalled builds the servedPlugin the client loads right after an install.
func servedForInstalled(mf installManifest) servedPlugin {
	return servedPlugin{
		ID: mf.ID, Name: mf.Name, Version: mf.Version, Icon: mf.Icon,
		Permissions: mf.Permissions, FirstParty: false, Enabled: true,
		Client: &servedArtifact{
			URL:  apiPrefix + "/plugins/" + mf.ID + "/" + mf.Client.Entry,
			Hash: mf.Client.Hash,
		},
	}
}
