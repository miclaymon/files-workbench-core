package indexer

import (
	"os"
	"path/filepath"
	"testing"
)

// buildTree writes a small fixture tree and returns its root.
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"annual_report_2024.pdf",
		"reports/q1-report.md",
		"reports/q2 report.md",  // space in name — must not break FTS parsing
		"reports/draft [v2].md", // brackets — FTS special chars
		"budget.xlsx",
		"src/main.go",
		"src/util.go",
		"node_modules/pkg/index.js", // should be excluded by DefaultExclude
	}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func indexTree(t *testing.T, root string) *Service {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	svc := NewService(store, NewPortableSource(DefaultExclude([]string{"node_modules"}), nil))
	if err := svc.IndexRoot(root); err != nil {
		t.Fatalf("index: %v", err)
	}
	return svc
}

func names(p ResultPage) map[string]bool {
	m := map[string]bool{}
	for _, r := range p.Results {
		m[r.Name] = true
	}
	return m
}

func TestSubstringSearch(t *testing.T) {
	svc := indexTree(t, buildTree(t))

	p, err := svc.Search(Query{Text: "report"})
	if err != nil {
		t.Fatal(err)
	}
	got := names(p)
	// substring "report" matches the pdf and every *report* under reports/
	for _, want := range []string{"annual_report_2024.pdf", "q1-report.md", "q2 report.md"} {
		if !got[want] {
			t.Errorf("substring 'report' missing %q; got %v", want, keys(got))
		}
	}
	if got["budget.xlsx"] {
		t.Errorf("budget.xlsx should not match 'report'")
	}
}

func TestExclusion(t *testing.T) {
	svc := indexTree(t, buildTree(t))
	p, _ := svc.Search(Query{Text: "index"})
	if names(p)["index.js"] {
		t.Errorf("node_modules/index.js should have been excluded, but it was indexed")
	}
}

func TestFTSSpecialCharsAreLiteral(t *testing.T) {
	svc := indexTree(t, buildTree(t))
	// A query containing FTS operators / brackets must be treated as a literal
	// substring, never as query syntax (no error, correct match).
	for _, q := range []string{"draft [v2]", "q2 report", `"quoted"`} {
		p, err := svc.Search(Query{Text: q})
		if err != nil {
			t.Errorf("query %q errored: %v", q, err)
		}
		_ = p
	}
	p, _ := svc.Search(Query{Text: "draft [v2]"})
	if !names(p)["draft [v2].md"] {
		t.Errorf("literal bracket substring 'draft [v2]' should match draft [v2].md; got %v", keys(names(p)))
	}
}

func TestScopeAndTypeFilter(t *testing.T) {
	root := buildTree(t)
	svc := indexTree(t, root)

	// Scope to src/ — only the .go files.
	p, _ := svc.Search(Query{Scope: filepath.Join(root, "src"), Sort: SortName})
	if len(p.Results) != 2 {
		t.Errorf("scope src/ expected 2 entries, got %d: %v", len(p.Results), keys(names(p)))
	}

	// Type filter: only .md.
	p, _ = svc.Search(Query{Types: []string{"md"}})
	for _, r := range p.Results {
		if r.Ext != "md" {
			t.Errorf("type filter md returned %q (ext %q)", r.Name, r.Ext)
		}
	}
	if len(p.Results) != 3 {
		t.Errorf("expected 3 .md files, got %d", len(p.Results))
	}
}

func TestShortQueryFallback(t *testing.T) {
	svc := indexTree(t, buildTree(t))
	// "go" is 2 chars — below the trigram threshold — must still match via LIKE.
	p, err := svc.Search(Query{Text: "go"})
	if err != nil {
		t.Fatal(err)
	}
	got := names(p)
	if !got["main.go"] || !got["util.go"] {
		t.Errorf("short query 'go' should match the .go files via fallback; got %v", keys(got))
	}
}

func TestUpsertAndDeleteSubtree(t *testing.T) {
	store, _ := Open(":memory:")
	defer store.Close()

	entries := []Entry{
		{Path: "/data/a.txt", Name: "a.txt", Ext: "txt"},
		{Path: "/data/sub/b.txt", Name: "b.txt", Ext: "txt"},
		{Path: "/data/sub/c.txt", Name: "c.txt", Ext: "txt"},
		{Path: "/other/d.txt", Name: "d.txt", Ext: "txt"},
	}
	for _, e := range entries {
		if err := store.Upsert(e); err != nil {
			t.Fatal(err)
		}
	}
	// Re-upsert a.txt with a new size — must update, not duplicate.
	if err := store.Upsert(Entry{Path: "/data/a.txt", Name: "a.txt", Ext: "txt", Size: 999}); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Search(Query{Text: "a.txt"})
	if len(p.Results) != 1 || p.Results[0].Size != 999 {
		t.Errorf("upsert should update in place; got %d results, size %d", len(p.Results), sizeOf(p))
	}

	// Delete the /data/sub subtree — removes b and c, keeps a and d.
	n, err := store.DeleteSubtree("/data/sub")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("DeleteSubtree('/data/sub') expected 2 rows, deleted %d", n)
	}
	all, _ := store.Search(Query{})
	got := names(all)
	if got["b.txt"] || got["c.txt"] {
		t.Errorf("subtree entries should be gone; got %v", keys(got))
	}
	if !got["a.txt"] || !got["d.txt"] {
		t.Errorf("sibling/parent entries should remain; got %v", keys(got))
	}
}

func TestStatus(t *testing.T) {
	svc := indexTree(t, buildTree(t))
	st, err := svc.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "ready" {
		t.Errorf("state = %q, want ready", st.State)
	}
	if st.FileCount == 0 {
		t.Errorf("file count should be > 0")
	}
	if len(st.Volumes) != 1 {
		t.Errorf("expected 1 volume, got %d", len(st.Volumes))
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sizeOf(p ResultPage) int64 {
	if len(p.Results) == 0 {
		return -1
	}
	return p.Results[0].Size
}
