package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startBudgetService starts a content-indexing service with a specific budget and
// a tight scanner cadence for fast tests.
func startBudgetService(t *testing.T, root string, budget int64) *Service {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	svc := NewService(store, NewPortableSource(nil, nil))
	svc.SetContentBudget(budget)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := svc.Start(ctx, []string{root}); err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestContentSizeBudget(t *testing.T) {
	root := t.TempDir()
	// Three ~40-byte text files; a 60-byte budget fits one, maybe part of a second.
	write(t, root, "a.txt", strings.Repeat("alpha ", 8))   // 48 bytes
	write(t, root, "b.txt", strings.Repeat("bravo ", 8))   // 48 bytes
	write(t, root, "c.txt", strings.Repeat("charlie ", 6)) // 48 bytes

	svc := startBudgetService(t, root, 60) // room for one file only

	// The scanner examines all three (so pending drains) but only indexes within budget.
	// Require the walk to have added the files first — ContentPending is vacuously 0
	// before any file exists, which would let the assertion run too early.
	waitFor(t, "budget scan settled", 5*time.Second, func() bool {
		st, _ := svc.Status()
		return st.FileCount >= 4 && st.ContentPending == 0 // 3 files + the root dir
	})
	st, _ := svc.Status()
	if st.ContentIndexed != 1 {
		t.Errorf("with a 60-byte budget over 3×48-byte files, expected 1 indexed, got %d", st.ContentIndexed)
	}
	if st.ContentBytes > 60 {
		t.Errorf("indexed bytes %d exceeded the budget of 60", st.ContentBytes)
	}
	if st.ContentBudget != 60 {
		t.Errorf("status budget = %d, want 60", st.ContentBudget)
	}
}

func TestBudgetAllowsReindexOfIndexedFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "kept.txt", "findme alpha")

	// Budget just fits kept.txt; index it first.
	svc := startBudgetService(t, root, 20)
	waitFor(t, "kept indexed", 5*time.Second, func() bool { return contentCount(svc, "findme") == 1 })

	// Now add a second file — over budget, so it should NOT be content-indexed.
	write(t, root, "extra.txt", "brandnew content")
	waitFor(t, "extra examined", 5*time.Second, func() bool {
		st, _ := svc.Status()
		return st.ContentPending == 0
	})
	if contentCount(svc, "brandnew") != 0 {
		t.Errorf("over-budget new file should not be content-indexed")
	}

	// Modifying the ALREADY-indexed file must still re-index (replace), budget or not.
	write(t, root, "kept.txt", "findme beta")
	waitFor(t, "kept reindexed", 5*time.Second, func() bool {
		return contentCount(svc, "beta") == 1 && contentCount(svc, "alpha") == 0
	})
}

func TestBodyBytesMigration(t *testing.T) {
	// A DB whose content_meta predates body_bytes should gain the column on open.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")

	// Create the pre-migration schema shape by hand, then close.
	old, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	old.db.Exec(`ALTER TABLE content_meta DROP COLUMN body_bytes`) // simulate an older DB
	old.Close()

	// Re-open — migrate() must re-add body_bytes without error, and content ops work.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen/migrate: %v", err)
	}
	defer s.Close()
	// Insert a file + index its content to exercise the body_bytes write/read path.
	s.Upsert(Entry{Path: "/x/f.txt", Name: "f.txt", Ext: "txt", Size: 5})
	var id int64
	s.db.QueryRow(`SELECT id FROM files WHERE path = '/x/f.txt'`).Scan(&id)
	if err := s.IndexContent(id, "hello world", "", 1, 5); err != nil {
		t.Fatalf("IndexContent after migration: %v", err)
	}
	if b := s.contentBytes(); b != int64(len("hello world")) {
		t.Errorf("contentBytes = %d, want %d", b, len("hello world"))
	}
}
