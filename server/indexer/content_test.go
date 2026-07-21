package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractText(t *testing.T) {
	dir := t.TempDir()

	textPath := filepath.Join(dir, "a.txt")
	os.WriteFile(textPath, []byte("the quick brown fox"), 0o644)
	if body, ok := extractText(textPath, 19, defaultMaxContentBytes); !ok || !strings.Contains(body, "brown fox") {
		t.Errorf("text file should extract; ok=%v body=%q", ok, body)
	}

	binPath := filepath.Join(dir, "b.bin")
	os.WriteFile(binPath, []byte{0x00, 0x01, 0x02, 'h', 'i', 0x00}, 0o644)
	if _, ok := extractText(binPath, 6, defaultMaxContentBytes); ok {
		t.Errorf("binary file (NUL bytes) should be skipped")
	}

	bigPath := filepath.Join(dir, "big.txt")
	os.WriteFile(bigPath, []byte("x"), 0o644)
	if _, ok := extractText(bigPath, 100, 10); ok { // size (100) exceeds cap (10)
		t.Errorf("oversize file should be skipped by the cap")
	}
}

func startContentService(t *testing.T, root string) *Service {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	svc := NewService(store, NewPortableSource(DefaultExclude([]string{"node_modules"}), nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := svc.Start(ctx, []string{root}); err != nil {
		t.Fatal(err)
	}
	return svc
}

func contentCount(svc *Service, text string) int {
	p, err := svc.Search(Query{Text: text, Content: true})
	if err != nil {
		return -1
	}
	return len(p.Results)
}

func TestContentSearch(t *testing.T) {
	root := t.TempDir()
	write(t, root, "notes/meeting.md", "discuss the quarterly synergy roadmap")
	write(t, root, "notes/recipe.txt", "combine flour sugar and cinnamon")
	write(t, root, "code/app.go", "package main // widget orchestration")
	// a binary file that happens to contain the word as bytes — must NOT be content-indexed
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), append([]byte("synergy"), 0x00, 0x01), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := startContentService(t, root)

	// Content indexing is background — wait for it to catch up.
	waitFor(t, "content indexed", 5*time.Second, func() bool {
		st, _ := svc.Status()
		return st.ContentIndexed >= 3 && st.ContentPending == 0
	})

	// A word that appears only in file CONTENT (not in any name) is found via content search.
	if n := contentCount(svc, "synergy"); n != 1 {
		t.Errorf("content search 'synergy' expected 1 (meeting.md only), got %d", n)
	}
	if n := contentCount(svc, "cinnamon"); n != 1 {
		t.Errorf("content search 'cinnamon' expected 1 (recipe.txt), got %d", n)
	}
	// AND-of-words semantics: both must be present.
	if n := contentCount(svc, "flour cinnamon"); n != 1 {
		t.Errorf("content 'flour cinnamon' expected 1, got %d", n)
	}
	if n := contentCount(svc, "flour synergy"); n != 0 {
		t.Errorf("content 'flour synergy' (different files) expected 0, got %d", n)
	}
	// The word 'synergy' in the binary blob must NOT be content-indexed.
	p, _ := svc.Search(Query{Text: "synergy", Content: true})
	for _, r := range p.Results {
		if strings.HasSuffix(r.Path, "blob.bin") {
			t.Errorf("binary blob.bin should not be content-indexed")
		}
	}
	// Name search still works independently and doesn't match content words.
	if n := countMatches(svc, "synergy"); n != 0 {
		t.Errorf("NAME search 'synergy' should be 0 (it's only in content), got %d", n)
	}
}

func TestContentReindexOnModify(t *testing.T) {
	root := t.TempDir()
	write(t, root, "doc.md", "original alpha content")
	svc := startContentService(t, root)

	waitFor(t, "initial content", 5*time.Second, func() bool { return contentCount(svc, "alpha") == 1 })

	// Rewrite with different content — the scanner must re-index (new word in, old out).
	write(t, root, "doc.md", "revised beta content")
	waitFor(t, "reindexed content", 5*time.Second, func() bool {
		return contentCount(svc, "beta") == 1 && contentCount(svc, "alpha") == 0
	})
}
