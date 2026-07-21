// Command fw-indexer is the standalone filesystem index service (see
// ../../../docs/INDEX.md). It owns the on-disk SQLite index, walks its configured
// roots, and serves a local query/status API that the core data server proxies.
//
// MVP scope: a one-shot portable walk of each root at startup, then serve queries.
// The incremental watcher, journal catch-up, and native backends land next, behind
// the same store — no API change.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"files-workbench/v2/indexer"
)

func main() {
	var (
		dbPath = flag.String("db", envOr("FW_INDEX_DB", ""), "index DB path (default: <FW_DATA_DIR>/index/index.db)")
		addr   = flag.String("addr", envOr("FW_INDEX_ADDR", "127.0.0.1:8010"), "listen address")
		roots  = flag.String("roots", os.Getenv("FW_INDEX_ROOTS"), "comma-separated roots to index (default: user home)")
		skip   = flag.String("skip", "node_modules,.git,.cache,.Trash", "comma-separated directory names to skip anywhere in the tree")
	)
	flag.Parse()

	resolvedDB := resolveDBPath(*dbPath)
	if err := os.MkdirAll(filepath.Dir(resolvedDB), 0o755); err != nil {
		log.Fatalf("[fw-indexer] cannot create index dir: %v", err)
	}
	store, err := indexer.Open(resolvedDB)
	if err != nil {
		log.Fatalf("[fw-indexer] open index: %v", err)
	}
	defer store.Close()

	src := indexer.NewPortableSource(indexer.DefaultExclude(splitCSV(*skip)),
		func(msg string) { log.Printf("[fw-indexer] %s", msg) })
	svc := indexer.NewService(store, src)
	svc.SetLogger(func(msg string) { log.Printf("[fw-indexer] %s", msg) })
	// Full-text content indexing is on by default; FW_INDEX_CONTENT=0 makes it a
	// name-only index (lighter — no background file reads).
	if os.Getenv("FW_INDEX_CONTENT") == "0" {
		svc.SetContentIndexing(false)
	}

	// Index + watch the configured roots in the background so the query API is up
	// immediately (results fill in as the walk progresses; live changes apply after).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	roolist := resolveRoots(*roots)
	if err := svc.Start(ctx, roolist); err != nil {
		log.Printf("[fw-indexer] start: %v", err)
	}
	log.Printf("[fw-indexer] indexing+watching %v", roolist)

	log.Printf("[fw-indexer] index=%s listening on %s", resolvedDB, *addr)
	srv := &http.Server{Addr: *addr, Handler: svc.Handler()}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[fw-indexer] serve: %v", err)
	}
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

// resolveRoots returns the configured roots. With none configured it indexes
// nothing (explicit-roots policy) — the service still runs and serves an empty
// index rather than silently walking the whole home directory.
func resolveRoots(csv string) []string { return splitCSV(csv) }

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
