package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func countMatches(svc *Service, text string) int {
	p, err := svc.Search(Query{Text: text})
	if err != nil {
		return -1
	}
	return len(p.Results)
}

// waitFor polls cond until it's true or the deadline passes — live index updates are
// eventually-consistent (fsnotify events are asynchronous).
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestLiveIndexUpdates(t *testing.T) {
	root := t.TempDir()
	write(t, root, "initial.txt", "x")

	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := NewService(store, NewPortableSource(DefaultExclude([]string{"node_modules"}), nil))

	// Collect the live deltas a subscriber sees.
	sub, unsub := svc.Subscribe()
	defer unsub()
	var mu sync.Mutex
	seen := map[string]map[ChangeOp]bool{} // base name → ops observed
	go func() {
		for c := range sub {
			mu.Lock()
			base := filepath.Base(c.Entry.Path)
			if seen[base] == nil {
				seen[base] = map[ChangeOp]bool{}
			}
			seen[base][c.Op] = true
			mu.Unlock()
		}
	}()
	sawOp := func(base string, op ChangeOp) bool {
		mu.Lock()
		defer mu.Unlock()
		return seen[base] != nil && seen[base][op]
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx, []string{root}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "initial walk", 3*time.Second, func() bool { return countMatches(svc, "initial") == 1 })

	// CREATE a file → indexed live.
	write(t, root, "created.txt", "y")
	waitFor(t, "live create", 3*time.Second, func() bool { return countMatches(svc, "created") == 1 })

	// MODIFY → new size reflected.
	write(t, root, "created.txt", "yyyyyy") // 6 bytes
	waitFor(t, "live modify", 3*time.Second, func() bool {
		p, _ := svc.Search(Query{Text: "created"})
		return len(p.Results) == 1 && p.Results[0].Size == 6
	})

	// A NEW SUBDIRECTORY with a file → auto-watched and indexed (the create-a-subtree path).
	write(t, root, "newdir/nested.txt", "z")
	waitFor(t, "new subdir indexed", 3*time.Second, func() bool { return countMatches(svc, "nested") == 1 })
	// And a file created in that new dir afterward is caught (proves the watch was added).
	write(t, root, "newdir/later.txt", "z")
	waitFor(t, "file in new subdir", 3*time.Second, func() bool { return countMatches(svc, "later") == 1 })

	// DELETE → removed from the index.
	if err := os.Remove(filepath.Join(root, "created.txt")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "live delete", 3*time.Second, func() bool { return countMatches(svc, "created") == 0 })

	// DELETE a whole subtree → all descendants gone.
	if err := os.RemoveAll(filepath.Join(root, "newdir")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "subtree delete", 3*time.Second, func() bool {
		return countMatches(svc, "nested") == 0 && countMatches(svc, "later") == 0
	})

	// The subscriber saw the create and delete deltas for created.txt.
	waitFor(t, "subscriber add delta", 2*time.Second, func() bool { return sawOp("created.txt", ChangeAdded) })
	waitFor(t, "subscriber remove delta", 2*time.Second, func() bool { return sawOp("created.txt", ChangeRemoved) })
}

func TestWatchExclusion(t *testing.T) {
	root := t.TempDir()
	store, _ := Open(":memory:")
	defer store.Close()
	svc := NewService(store, NewPortableSource(DefaultExclude([]string{"node_modules"}), nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx, []string{root}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "watch ready", 2*time.Second, func() bool { s, _ := svc.Status(); return s.State == "ready" })

	// A file created under an excluded dir must not be indexed.
	write(t, root, "node_modules/pkg/junk.js", "x")
	// Give the watcher a moment; then assert it stayed out.
	time.Sleep(300 * time.Millisecond)
	if countMatches(svc, "junk") != 0 {
		t.Errorf("file under node_modules should not be indexed live")
	}
}
