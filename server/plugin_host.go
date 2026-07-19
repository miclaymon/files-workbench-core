package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	extism "github.com/extism/go-sdk"
)

// ── Server-side plugins (sandboxed WASM backends) ─────────────────────────────
//
// A plugin can ship its own backend as a WebAssembly module compiled from JS/TS
// (via the extism JS PDK). We run it in-process with extism/wazero — a hard
// sandbox: the module has no ambient authority. Its only way to touch the OS is
// through the host functions registered below, each gated by the permissions the
// plugin declared in its `server` block.
//
// The client calls a plugin backend through one generic endpoint —
// POST /_api/v1/plugins/<id>/rpc { method, params } — so a new server plugin needs
// zero new Go code. This is what retires the bespoke scm.go handlers: the git
// logic now lives in a WASM plugin that calls the host `exec`/`fs` functions.

// serverPluginDef is the `server` block in config/plugins/<id>/plugin.json.
type serverPluginDef struct {
	Entry       string   `json:"entry"`       // wasm file, relative to the plugin dir
	Runtime     string   `json:"runtime"`     // only "wasm-js" today
	Permissions []string `json:"permissions"` // e.g. ["exec:git", "fs:read"]
}

// serverPlugin is one loaded, compiled WASM backend.
type serverPlugin struct {
	id       string
	perms    map[string]bool
	compiled *extism.CompiledPlugin
	mu       sync.Mutex     // extism instances aren't concurrency-safe — guard the instance
	inst     *extism.Plugin // lazily instantiated on first RPC
}

var (
	serverPlugins   = map[string]*serverPlugin{}
	serverPluginsMu sync.RWMutex
)

// i64in/i64out are the host-function signature the extism JS PDK imports use: one
// i64 memory offset in (a JSON params string), one i64 offset out (a JSON result).
var (
	i64in  = []extism.ValueType{extism.ValueTypeI64}
	i64out = []extism.ValueType{extism.ValueTypeI64}
)

// loadServerPlugins scans config/plugins/<id>/plugin.json for a `server` block and
// compiles each backend. Called once at startup, after loadPlugins.
func loadServerPlugins() {
	dir := filepath.Join(configDir, "plugins")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pdir := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(filepath.Join(pdir, "plugin.json"))
		if err != nil {
			continue
		}
		var m pluginManifest
		if err := json.Unmarshal(data, &m); err != nil || m.Server == nil {
			continue
		}
		if err := loadOneServerPlugin(pdir, m); err != nil {
			log.Printf("server-plugins: %s: %v", e.Name(), err)
		}
	}
}

func loadOneServerPlugin(pdir string, m pluginManifest) error {
	id := m.ID
	if id == "" {
		id = filepath.Base(pdir)
	}
	wasmPath := filepath.Join(pdir, filepath.FromSlash(m.Server.Entry))
	if _, err := os.Stat(wasmPath); err != nil {
		return fmt.Errorf("wasm not found: %s", wasmPath)
	}
	sp := &serverPlugin{id: id, perms: map[string]bool{}}
	for _, p := range m.Server.Permissions {
		sp.perms[p] = true
	}

	manifest := extism.Manifest{Wasm: []extism.Wasm{extism.WasmFile{Path: wasmPath}}}
	compiled, err := extism.NewCompiledPlugin(
		context.Background(), manifest,
		extism.PluginConfig{EnableWasi: true},
		sp.hostFunctions(),
	)
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	sp.compiled = compiled

	serverPluginsMu.Lock()
	serverPlugins[id] = sp
	serverPluginsMu.Unlock()
	log.Printf("server-plugins: loaded %q (%d permission(s))", id, len(sp.perms))
	return nil
}

// handlePluginRpc is the single, generic broker: it forwards { method, params } to
// the plugin's `handle` export and returns its JSON output verbatim (the SDK wraps
// results as { ok, result } / { ok:false, error }; the client SDK unwraps).
func handlePluginRpc(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	serverPluginsMu.RLock()
	sp := serverPlugins[id]
	serverPluginsMu.RUnlock()
	if sp == nil {
		jsonErr(w, http.StatusNotFound, "no server plugin: "+id)
		return
	}

	var body struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Method == "" {
		jsonErr(w, http.StatusBadRequest, "method required")
		return
	}

	out, err := sp.call(r.Context(), body.Method, body.Params)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// call instantiates the plugin lazily, then invokes its `handle` export. extism
// instances aren't concurrency-safe, so calls are serialized per plugin.
func (sp *serverPlugin) call(ctx context.Context, method string, params json.RawMessage) ([]byte, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.inst == nil {
		inst, err := sp.compiled.Instance(ctx, extism.PluginInstanceConfig{})
		if err != nil {
			return nil, fmt.Errorf("instantiate: %w", err)
		}
		sp.inst = inst
		if inst.FunctionExists("plugin_init") {
			_, _, _ = inst.CallWithContext(ctx, "plugin_init", nil)
		}
	}

	input, _ := json.Marshal(struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{method, params})

	exit, out, err := sp.inst.CallWithContext(ctx, "handle", input)
	if err != nil {
		return nil, fmt.Errorf("plugin %s.%s failed (exit %d): %w", sp.id, method, exit, err)
	}
	return out, nil
}

// ── Host functions ────────────────────────────────────────────────────────────
// All server plugins get the same set registered; each call re-checks the plugin's
// granted permissions, so an ungranted capability is inert (returns { error }) even
// though the import links. Namespace is the default "extism:host/user" (what the JS
// PDK imports from) — do not SetNamespace.

func (sp *serverPlugin) hostFunctions() []extism.HostFunction {
	return []extism.HostFunction{
		extism.NewHostFunctionWithStack("host_exec", sp.hostExec, i64in, i64out),
		extism.NewHostFunctionWithStack("host_fs_stat", sp.hostFsStat, i64in, i64out),
		extism.NewHostFunctionWithStack("host_fs_read_dir", sp.hostFsReadDir, i64in, i64out),
		extism.NewHostFunctionWithStack("host_fs_read_file", sp.hostFsReadFile, i64in, i64out),
		extism.NewHostFunctionWithStack("host_log", sp.hostLog, i64in, i64out),
	}
}

// hostCall reads the JSON params string at stack[0], runs fn, and writes the JSON
// result back, leaving its offset in stack[0]. Every host function shares this shape.
func hostCall(p *extism.CurrentPlugin, stack []uint64, fn func(raw []byte) any) {
	in, err := p.ReadString(stack[0])
	if err != nil {
		in = ""
	}
	result := fn([]byte(in))
	out, err := json.Marshal(result)
	if err != nil {
		out = []byte(`{"error":"host: cannot encode result"}`)
	}
	off, err := p.WriteString(string(out))
	if err != nil {
		stack[0] = 0
		return
	}
	stack[0] = off
}

func (sp *serverPlugin) hostExec(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
	hostCall(p, stack, func(raw []byte) any {
		var req struct {
			Bin  string   `json:"bin"`
			Args []string `json:"args"`
			Cwd  string   `json:"cwd"`
		}
		_ = json.Unmarshal(raw, &req)
		if !sp.perms["exec:"+req.Bin] {
			return map[string]any{"error": "permission denied: exec:" + req.Bin}
		}
		// Defense in depth: refuse to run inside a blacklisted working directory.
		if req.Cwd != "" && isExcluded(req.Cwd, nil) {
			return map[string]any{"error": "path is blacklisted"}
		}
		cmd := exec.Command(req.Bin, req.Args...)
		if req.Cwd != "" {
			cmd.Dir = req.Cwd
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		return map[string]any{"stdout": stdout.String(), "stderr": stderr.String(), "code": code}
	})
}

func (sp *serverPlugin) hostFsStat(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
	hostCall(p, stack, func(raw []byte) any {
		path, ok := sp.readablePath(raw)
		if !ok {
			return map[string]any{"error": "permission denied: fs:read"}
		}
		if isExcluded(path, nil) {
			return map[string]any{"error": "path is blacklisted"}
		}
		info, err := os.Stat(path)
		if err != nil {
			return map[string]any{"exists": false}
		}
		return map[string]any{"exists": true, "isDir": info.IsDir(), "size": info.Size(), "name": info.Name()}
	})
}

func (sp *serverPlugin) hostFsReadDir(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
	hostCall(p, stack, func(raw []byte) any {
		path, ok := sp.readablePath(raw)
		if !ok {
			return map[string]any{"error": "permission denied: fs:read"}
		}
		if isExcluded(path, nil) {
			return map[string]any{"error": "path is blacklisted"}
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, map[string]any{"name": e.Name(), "isDir": e.IsDir()})
		}
		return map[string]any{"entries": out}
	})
}

func (sp *serverPlugin) hostFsReadFile(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
	hostCall(p, stack, func(raw []byte) any {
		path, ok := sp.readablePath(raw)
		if !ok {
			return map[string]any{"error": "permission denied: fs:read"}
		}
		if isExcluded(path, nil) {
			return map[string]any{"error": "path is blacklisted"}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		return map[string]any{"content": string(data)}
	})
}

func (sp *serverPlugin) hostLog(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
	hostCall(p, stack, func(raw []byte) any {
		var req struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(raw, &req)
		log.Printf("plugin:%s %s", sp.id, req.Msg)
		return map[string]any{"ok": true}
	})
}

// readablePath decodes { path } and enforces the fs:read permission.
func (sp *serverPlugin) readablePath(raw []byte) (string, bool) {
	if !sp.perms["fs:read"] {
		return "", false
	}
	var req struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(raw, &req)
	return req.Path, true
}
