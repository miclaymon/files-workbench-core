package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

const version = "v1"
const apiPrefix = "/_api/" + version

// Path roots, resolved at startup. In a packaged build the FW_* env vars (set by
// the Electron main process) point these at app resources and a writable user-data
// dir; running unbundled (go run / a local binary in the repo) they fall back to
// the repo layout so dev needs no configuration.
//
//	configDir — read-only bundled config: preferences schema/defaults, plugins.
//	dataDir   — writable user data: user-preferences.json.
//	logsDir   — writable logs: perf.log.
//	blacklistPath — the blacklist.yaml file.
var (
	repoRoot      string
	configDir     string
	dataDir       string
	logsDir       string
	blacklistPath string
)

// envOr returns the environment value for key, or fallback when it is unset/empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	// Dev fallback: the package root is one level up from server/. When running via
	// `go run`, the binary is in a temp dir — use the source dir instead. A host app
	// points the server at ITS config/data/plugin roots via the FW_* env vars below
	// (Files Workbench does this in its dev:server script and in Electron's main.js);
	// the package-relative fallbacks are a last resort for running standalone.
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	dir := filepath.Dir(exe)
	if _, src, _, ok := runtime.Caller(0); ok {
		dir = filepath.Dir(src) // server/
	}
	repoRoot, _ = filepath.Abs(filepath.Join(dir, ".."))

	// Resolve path roots; FW_* env vars (packaged build) override the dev fallbacks.
	configDir = envOr("FW_CONFIG_DIR", filepath.Join(repoRoot, "config"))
	dataDir = envOr("FW_DATA_DIR", filepath.Join(repoRoot, "config"))
	logsDir = envOr("FW_LOGS_DIR", filepath.Join(repoRoot, "server", "logs"))
	blacklistPath = envOr("FW_BLACKLIST", filepath.Join(repoRoot, "server", "blacklist.yaml"))

	// Runtime plugin artifacts: first-party built output (FW_PLUGINS_DIR, or the repo
	// .fw/plugins dir in dev) + user-installed third-party plugins (writable data dir).
	pluginsDistDir = envOr("FW_PLUGINS_DIR", filepath.Join(repoRoot, ".fw", "plugins"))
	thirdPartyPluginsDir = filepath.Join(dataDir, "plugins")
	pluginRegistryURL = os.Getenv("FW_PLUGIN_REGISTRY") // remote plugin index (optional)

	if err := loadBlacklist(blacklistPath); err != nil {
		log.Printf("warn: could not load blacklist: %v", err)
	}

	loadPlugins()
	loadServerPlugins()

	dataPort := os.Getenv("PORT")
	if dataPort == "" {
		dataPort = "8001"
	}
	controlPort := os.Getenv("CONTROL_PORT")
	if controlPort == "" {
		controlPort = "8002"
	}

	dataMux := http.NewServeMux()
	controlMux := http.NewServeMux()
	registerDataRoutes(dataMux)
	registerControlRoutes(controlMux)

	var wg sync.WaitGroup
	serve := func(label, port string, h http.Handler) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("Files Workbench %s server (%s) listening on :%s", version, label, port)
			if err := http.ListenAndServe(":"+port, cors(h)); err != nil {
				log.Fatalf("%s server: %v", label, err)
			}
		}()
	}

	serve("data", dataPort, dataMux)
	serve("control", controlPort, controlMux)
	wg.Wait()
}

// cors adds permissive CORS headers to every response.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// registerDataRoutes registers all read-only GET endpoints on the data server.
func registerDataRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET "+apiPrefix+"/app/init", handleAppInit)

	// Filesystem — reads
	mux.HandleFunc("GET "+apiPrefix+"/fs/stat", handleFsStat)
	mux.HandleFunc("GET "+apiPrefix+"/fs/list_dir", handleFsListDir)
	mux.HandleFunc("GET "+apiPrefix+"/fs/dir_size", handleFsDirSize)
	mux.HandleFunc("GET "+apiPrefix+"/fs/preview", handleFsPreview)
	mux.HandleFunc("GET "+apiPrefix+"/fs/archive/capabilities", handleArchiveCapabilities)
	mux.HandleFunc("GET "+apiPrefix+"/fs/archive/ls", handleFsArchiveLs)
	mux.HandleFunc("GET "+apiPrefix+"/fs/customization", handleFsCustomizationGet)
	mux.HandleFunc("GET "+apiPrefix+"/fs/permissions", handleFsPermissions)
	mux.HandleFunc("GET "+apiPrefix+"/fs/checksums", handleFsChecksums)

	// Media
	mux.HandleFunc("GET "+apiPrefix+"/media/capabilities", handleMediaCapabilities)
	mux.HandleFunc("GET "+apiPrefix+"/media/image", handleMediaImage)
	mux.HandleFunc("GET "+apiPrefix+"/media/thumbnail", handleMediaThumbnail)
	mux.HandleFunc("GET "+apiPrefix+"/media/preview", handleMediaPreview)
	mux.HandleFunc("GET "+apiPrefix+"/media/preview/text", handleMediaPreviewText)
	mux.HandleFunc("GET "+apiPrefix+"/media/metadata", handleMediaMetadata)
	mux.HandleFunc("GET "+apiPrefix+"/media/artwork", handleMediaArtwork)
	mux.HandleFunc("GET "+apiPrefix+"/media/exe_icon", handleMediaExeIcon)
	mux.HandleFunc("GET "+apiPrefix+"/media/exe_info", handleMediaExeInfo)
	mux.HandleFunc("GET "+apiPrefix+"/media/exif", handleMediaExif)
	mux.HandleFunc("GET "+apiPrefix+"/media/audio_tags", handleMediaAudioTags)

	// Explorer
	mux.HandleFunc("GET "+apiPrefix+"/Explorer/categories", handleExplorerCategories)
	mux.HandleFunc("GET "+apiPrefix+"/Explorer/root", handleExplorerRoot)
	mux.HandleFunc("GET "+apiPrefix+"/Explorer/home", handleExplorerHome)
	mux.HandleFunc("GET "+apiPrefix+"/Explorer/drives", handleExplorerDrives)
	mux.HandleFunc("GET "+apiPrefix+"/Explorer", handleExplorer)

	// Preferences — reads
	mux.HandleFunc("GET "+apiPrefix+"/preferences/schema", handlePreferencesSchema)
	mux.HandleFunc("GET "+apiPrefix+"/preferences", handlePreferencesGet)

	// Icon packs
	mux.HandleFunc("GET "+apiPrefix+"/icons/manifest", handleIconsManifest)
	mux.HandleFunc("GET "+apiPrefix+"/icons/svg", handleIconsSvg)

	// Server plugins — generic RPC broker (read-side). One handler serves every
	// plugin backend; a new server plugin needs no new Go code.
	mux.HandleFunc("POST "+apiPrefix+"/plugins/{id}/rpc", handlePluginRpc)

	// Runtime plugin loading — discovery manifest + artifact serving (client.js, wasm).
	mux.HandleFunc("GET "+apiPrefix+"/plugins/manifest", handlePluginsManifest)
	mux.HandleFunc("GET "+apiPrefix+"/plugins/registry", handlePluginRegistry)
	mux.HandleFunc("GET "+apiPrefix+"/plugins/{id}/{artifact}", handlePluginArtifact)
}

// registerControlRoutes registers all mutating POST/PUT endpoints on the control server.
func registerControlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", handleHealth)

	// Filesystem — writes
	mux.HandleFunc("POST "+apiPrefix+"/fs/open_with_system", handleFsOpenWithSystem)
	mux.HandleFunc("POST "+apiPrefix+"/fs/open_terminal", handleFsOpenTerminal)
	mux.HandleFunc("POST "+apiPrefix+"/fs/create_file", handleFsCreateFile)
	mux.HandleFunc("POST "+apiPrefix+"/fs/create_dir", handleFsCreateDir)
	mux.HandleFunc("POST "+apiPrefix+"/fs/write_file", handleFsWriteFile)
	mux.HandleFunc("POST "+apiPrefix+"/fs/rename", handleFsRename)
	mux.HandleFunc("POST "+apiPrefix+"/fs/move", handleFsMove)
	mux.HandleFunc("POST "+apiPrefix+"/fs/copy", handleFsCopy)
	mux.HandleFunc("POST "+apiPrefix+"/fs/delete", handleFsDelete)
	mux.HandleFunc("POST "+apiPrefix+"/fs/delete/elevated", handleFsDeleteElevated)
	mux.HandleFunc("POST "+apiPrefix+"/fs/trash", handleFsTrash)
	mux.HandleFunc("POST "+apiPrefix+"/fs/trash/elevated", handleFsTrashElevated)
	mux.HandleFunc("POST "+apiPrefix+"/fs/compress", handleFsCompress)
	mux.HandleFunc("POST "+apiPrefix+"/fs/decompress", handleFsDecompress)
	mux.HandleFunc("PUT "+apiPrefix+"/fs/customization", handleFsCustomizationPut)
	mux.HandleFunc("PATCH "+apiPrefix+"/fs/customization", handleFsCustomizationPatch)
	mux.HandleFunc("POST "+apiPrefix+"/fs/pin", handleFsPin)

	// Preferences — writes
	mux.HandleFunc("PUT "+apiPrefix+"/preferences", handlePreferencesPut)

	// Perf logging
	mux.HandleFunc("POST "+apiPrefix+"/perf", handlePerf)

	// Server plugins — generic RPC broker (write-side, same handler).
	mux.HandleFunc("POST "+apiPrefix+"/plugins/{id}/rpc", handlePluginRpc)

	// Third-party plugin management (install / uninstall / enable-disable).
	mux.HandleFunc("POST "+apiPrefix+"/plugins/install", handlePluginInstall)
	mux.HandleFunc("DELETE "+apiPrefix+"/plugins/{id}", handlePluginUninstall)
	mux.HandleFunc("POST "+apiPrefix+"/plugins/{id}/enabled", handlePluginSetEnabled)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]string{"status": "ok"})
}

func handleAppInit(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	jsonOK(w, map[string]string{"homePath": home, "platform": runtime.GOOS})
}

// jsonOK writes a JSON 200 response.
func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

// jsonErr writes a JSON error response.
func jsonErr(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}
