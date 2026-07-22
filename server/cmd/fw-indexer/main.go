// Command fw-indexer is the standalone filesystem index service (see
// ../../../docs/INDEX.md). It owns the on-disk SQLite index, walks its configured
// roots, and serves a local query/status API that the core data server proxies.
//
// Two lifecycles (see ../../../docs/DAEMON_PLAN.md):
//   - App-child (default): core spawns and supervises it; it dies with the app.
//   - OS daemon (Phase 4): registered with the OS service manager via the
//     install/uninstall/service-status subcommands, so it stays warm across app
//     restarts. Core adopts an already-running daemon instead of spawning a child.
//
// Usage:
//
//	fw-indexer [flags]                 run the indexer (default)
//	fw-indexer install [flags]         register + start as an OS-managed daemon
//	fw-indexer uninstall               stop + deregister the daemon
//	fw-indexer service-status          report the daemon's install/run state
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"files-workbench/v2/indexer"
)

// runConfig is the resolved configuration for one indexer run — shared by the normal
// foreground path and the Windows service handler (run_windows.go).
type runConfig struct {
	DB    string // resolved index DB path
	Addr  string // listen address
	Roots string // comma-separated roots
	Skip  string // comma-separated dir names to skip
}

func main() {
	// Subcommand dispatch (before flag.Parse, which only handles the run flags).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install", "uninstall", "service-status":
			runServiceCommand(os.Args[1], os.Args[2:])
			return
		}
	}

	cfg := parseRunFlags(os.Args[1:])

	// On Windows, if the SCM launched us, run under the service control handler
	// instead of the foreground path. A no-op (returns false) on every other OS and
	// when launched interactively.
	if runAsServiceIfManaged(cfg) {
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runIndexer(ctx, cfg); err != nil {
		log.Fatalf("[fw-indexer] %v", err)
	}
}

// parseRunFlags resolves the run configuration from flags + the FW_* environment.
func parseRunFlags(args []string) runConfig {
	fs := flag.NewFlagSet("fw-indexer", flag.ExitOnError)
	dbPath := fs.String("db", envOr("FW_INDEX_DB", ""), "index DB path (default: <FW_DATA_DIR>/index/index.db)")
	addr := fs.String("addr", envOr("FW_INDEX_ADDR", "127.0.0.1:8010"), "listen address")
	roots := fs.String("roots", os.Getenv("FW_INDEX_ROOTS"), "comma-separated roots to index")
	skip := fs.String("skip", "node_modules,.git,.cache,.Trash", "comma-separated directory names to skip anywhere in the tree")
	fs.Parse(args)
	return runConfig{DB: resolveDBPath(*dbPath), Addr: *addr, Roots: *roots, Skip: *skip}
}

// runIndexer opens the store, starts indexing+watching the roots, and serves the
// query API until ctx is cancelled. Extracted so the Windows service handler can
// drive the exact same run with SCM-sourced cancellation.
func runIndexer(ctx context.Context, cfg runConfig) error {
	if err := os.MkdirAll(filepath.Dir(cfg.DB), 0o755); err != nil {
		return err
	}
	store, err := indexer.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer store.Close()

	logf := func(msg string) { log.Printf("[fw-indexer] %s", msg) }
	src := indexer.SelectSource(indexer.DefaultExclude(splitCSV(cfg.Skip)), logf)
	svc := indexer.NewService(store, src)
	svc.SetLogger(logf)
	// Full-text content indexing is on by default; FW_INDEX_CONTENT=0 makes it a
	// name-only index (lighter — no background file reads).
	if os.Getenv("FW_INDEX_CONTENT") == "0" {
		svc.SetContentIndexing(false)
	}
	// FW_INDEX_CONTENT_BUDGET caps the bytes of indexed text (0 = unlimited).
	if b := os.Getenv("FW_INDEX_CONTENT_BUDGET"); b != "" {
		if n, err := strconv.ParseInt(b, 10, 64); err == nil {
			svc.SetContentBudget(n)
		}
	}

	roots := splitCSV(cfg.Roots)
	if err := svc.Start(ctx, roots); err != nil {
		log.Printf("[fw-indexer] start: %v", err)
	}
	log.Printf("[fw-indexer] indexing+watching %v", roots)

	log.Printf("[fw-indexer] index=%s listening on %s", cfg.DB, cfg.Addr)
	srv := &http.Server{Addr: cfg.Addr, Handler: svc.Handler()}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// resolveDBPath honors an explicit --db, else FW_DATA_DIR/index/index.db, else a
// package-relative fallback for standalone runs (mirrors the server's FW_* contract).
func resolveDBPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if dataDir := os.Getenv("FW_DATA_DIR"); dataDir != "" {
		return filepath.Join(dataDir, "index", "index.db")
	}
	return filepath.Join(".fw", "index", "index.db")
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
