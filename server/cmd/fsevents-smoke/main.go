//go:build darwin

// Command fsevents-smoke is a minimal, standalone tool for validating the raw
// FSEvents cgo layer (server/indexer/fsevents_darwin.go) in isolation from the rest
// of the indexer. It is the first thing to run when picking up the macOS backend —
// see docs/MACOS_BACKEND_PLAN.md, step 1.
//
// It watches the given paths via indexer.DebugWatchFSEvents and prints every raw
// event (path, flags, event ID) it receives — nothing more. It does not touch the
// SQLite store, the Change/Entry mapping, or cursor persistence, so a failure here
// isolates the problem to stream setup/the callback itself rather than the rest of
// source_darwin.go.
//
// Usage:
//
//	go run ./cmd/fsevents-smoke <path> [<path> ...]
//
// Then, in another terminal, create/modify/rename/delete files under a watched path
// and confirm events show up with sane paths and human-readable flag names. Ctrl-C
// to stop.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"files-workbench/v2/indexer"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fsevents-smoke <path> [<path> ...]")
		os.Exit(2)
	}
	paths := os.Args[1:]

	fmt.Printf("watching %d path(s):\n", len(paths))
	for _, p := range paths {
		fmt.Println("  " + p)
	}
	fmt.Println("(Ctrl-C to stop)")

	stop, err := indexer.DebugWatchFSEvents(paths, func(path string, flags uint32, eventID uint64) {
		ts := time.Now().Format("15:04:05.000")
		fmt.Printf("[%s] id=%d flags=%s path=%s\n", ts, eventID, describeFlags(flags), path)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "DebugWatchFSEvents failed:", err)
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Println("\nstopping...")
	stop()
}

// describeFlags renders the bits fsevents_darwin.go defines as plain hex constants
// (flagItemCreated etc.) — duplicated here as raw literals rather than importing
// the unexported constants, since this command is meant to be a dependency-light,
// easy-to-read cross-check, not another consumer of package indexer's internals.
// If this list drifts from fsevents_darwin.go's, that's a signal to reconcile them.
func describeFlags(f uint32) string {
	names := []struct {
		bit  uint32
		name string
	}{
		{0x00000001, "MustScanSubDirs"},
		{0x00000002, "UserDropped"},
		{0x00000004, "KernelDropped"},
		{0x00000008, "EventIdsWrapped"},
		{0x00000010, "HistoryDone"},
		{0x00000020, "RootChanged"},
		{0x00000040, "Mount"},
		{0x00000080, "Unmount"},
		{0x00000100, "ItemCreated"},
		{0x00000200, "ItemRemoved"},
		{0x00000400, "ItemInodeMetaMod"},
		{0x00000800, "ItemRenamed"},
		{0x00001000, "ItemModified"},
		{0x00002000, "ItemFinderInfoMod"},
		{0x00004000, "ItemChangeOwner"},
		{0x00008000, "ItemXattrMod"},
		{0x00010000, "ItemIsFile"},
		{0x00020000, "ItemIsDir"},
		{0x00040000, "ItemIsSymlink"},
		{0x00080000, "OwnEvent"},
	}
	if f == 0 {
		return "none"
	}
	var hit []string
	for _, n := range names {
		if f&n.bit != 0 {
			hit = append(hit, n.name)
		}
	}
	if len(hit) == 0 {
		return fmt.Sprintf("0x%x", f)
	}
	return strings.Join(hit, "|")
}
