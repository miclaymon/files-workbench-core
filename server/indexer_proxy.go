package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// The search index runs as a separate process (fw-indexer — see
// ../docs/INDEX.md). Core owns that child: it spawns and supervises it, and proxies
// its query/status/change-feed endpoints under /_api/v1/ so the app keeps talking
// only to core. When the indexer isn't available (e.g. a dev checkout where the
// binary hasn't been built), search endpoints return a clean 503 rather than
// failing the whole server — everything else keeps working.

type indexManager struct {
	addr  string
	proxy *httputil.ReverseProxy

	mu       sync.Mutex
	cmd      *exec.Cmd
	shutdown bool
	adopted  bool // an OS-managed daemon was already running — we proxy but don't own it
}

// startIndexManager configures the proxy and, if the fw-indexer binary can be found,
// launches and supervises it. It never blocks and never fails startup.
func startIndexManager() *indexManager {
	addr := envOr("FW_INDEX_ADDR", "127.0.0.1:8010")
	target, _ := url.Parse("http://" + addr)

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Flush every write immediately so the SSE change feed streams (default buffering
	// would hold events until the response closed).
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeIndexUnavailable(w)
	}

	m := &indexManager{addr: addr, proxy: proxy}

	// Connect-or-spawn (Phase 4 — see ../docs/DAEMON_PLAN.md). If a fw-indexer daemon
	// is already answering on addr — an OS-managed service (systemd/launchd/SCM), or a
	// leftover from a previous run — adopt it: proxy to it, but never supervise or kill
	// it (its lifecycle belongs to the OS). This is checked before the roots gate on
	// purpose: an OS daemon carries its own FW_INDEX_ROOTS in its service definition,
	// so core can front a running index even when core itself wasn't given roots.
	if probeIndexerHealth(addr) {
		log.Printf("[index] adopting already-running indexer at %s (OS-managed — not supervising)", addr)
		m.adopted = true
		return m
	}

	// Explicit-roots policy: index only what the host configures (FW_INDEX_ROOTS).
	// With no roots we never spawn the indexer — search reports unavailable rather
	// than quietly indexing a user's entire home directory.
	if os.Getenv("FW_INDEX_ROOTS") == "" {
		log.Printf("[index] FW_INDEX_ROOTS not set — search index disabled")
		return m
	}
	bin := findIndexerBinary()
	if bin == "" {
		log.Printf("[index] fw-indexer binary not found — search disabled "+
			"(set FW_INDEX_BIN, or build it to %s)", filepath.Join(repoRoot, "server"))
		return m
	}
	go m.supervise(bin)
	return m
}

// probeIndexerHealth reports whether a fw-indexer is already serving on addr, via a
// short-timeout GET /health. Used by connect-or-spawn to decide adopt vs. spawn.
func probeIndexerHealth(addr string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// supervise runs the indexer and restarts it if it exits unexpectedly (bounded
// backoff), until stop() is called.
func (m *indexManager) supervise(bin string) {
	backoff := time.Second
	for {
		m.mu.Lock()
		if m.shutdown {
			m.mu.Unlock()
			return
		}
		cmd := exec.Command(bin, "--addr", m.addr)
		cmd.Env = append(os.Environ(),
			"FW_DATA_DIR="+dataDir,
			"FW_INDEX_ROOTS="+os.Getenv("FW_INDEX_ROOTS"),
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			m.mu.Unlock()
			log.Printf("[index] start failed: %v (search disabled)", err)
			return
		}
		m.cmd = cmd
		m.mu.Unlock()
		log.Printf("[index] fw-indexer started (pid %d, addr %s)", cmd.Process.Pid, m.addr)

		err := cmd.Wait()

		m.mu.Lock()
		down := m.shutdown
		m.mu.Unlock()
		if down {
			return
		}
		log.Printf("[index] fw-indexer exited (%v) — restarting in %s", err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// stop terminates the indexer child (called on core shutdown so it isn't orphaned).
// An adopted OS-managed daemon is left running — core never spawned it, so killing
// it on app quit would defeat the whole point of the daemon (cross-session warmth).
func (m *indexManager) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdown = true
	if m.adopted {
		return
	}
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Process.Kill()
	}
}

// proxyHandler forwards the request to a fixed indexer path (e.g. "/search"),
// preserving the query string.
func (m *indexManager) proxyHandler(indexerPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = indexerPath
		m.proxy.ServeHTTP(w, r2)
	}
}

func writeIndexUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(map[string]string{"error": "search index unavailable"})
}

// findIndexerBinary locates fw-indexer: an explicit override, then next to the core
// executable (packaged), then the dev-built location under server/.
func findIndexerBinary() string {
	if b := os.Getenv("FW_INDEX_BIN"); b != "" {
		return b
	}
	name := "fw-indexer"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	candidates = append(candidates, filepath.Join(repoRoot, "server", name))
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// registerIndexRoutes wires the proxied search surface onto the data mux.
func registerIndexRoutes(mux *http.ServeMux, m *indexManager) {
	mux.HandleFunc("GET "+apiPrefix+"/search", m.proxyHandler("/search"))
	mux.HandleFunc("GET "+apiPrefix+"/index/status", m.proxyHandler("/status"))
	mux.HandleFunc("GET "+apiPrefix+"/index/subscribe", m.proxyHandler("/subscribe"))
}
