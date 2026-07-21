// Package indexer is the filesystem search index for Files Workbench (see
// docs/INDEX.md). It maintains a searchable catalog of the filesystem behind one
// normalized interface, so the rest of the app queries "find files matching X"
// without knowing whether the answer came from a raw NTFS scan, macOS Spotlight,
// or (today) our own portable walk into SQLite FTS5.
//
// This file defines the OS-agnostic shapes the whole system binds to — they are
// the contract, so keep them free of platform detail.
package indexer

import "time"

// Entry is one catalogued filesystem object. The portable walker populates the
// fields it can read from a stdlib DirEntry; native backends (USN/MFT, Spotlight)
// refine the best-effort ones (Created, and later Content/Meta) behind the same shape.
type Entry struct {
	Path     string    `json:"path"`     // absolute, forward-slash normalized
	Name     string    `json:"name"`     // base name
	Ext      string    `json:"ext"`      // lowercased extension without the dot ("" for none/dirs)
	Size     int64     `json:"size"`     // bytes (0 for dirs)
	Modified time.Time `json:"modified"` // mtime
	Created  time.Time `json:"created"`  // best-effort; equals Modified until a native backend supplies birth time
	IsDir    bool      `json:"isDir"`
	VolumeID string    `json:"volumeId"` // for per-volume cursors/coverage
}

// MatchMode is how Query.Text is interpreted against name/path.
type MatchMode string

const (
	MatchSubstring MatchMode = "substring" // default — anywhere in name or path (the "Everything" experience)
	MatchPrefix    MatchMode = "prefix"
	MatchGlob      MatchMode = "glob"
)

// SortField orders results. Relevance is only meaningful for a text query (BM25).
type SortField string

const (
	SortName      SortField = "name"
	SortPath      SortField = "path"
	SortSize      SortField = "size"
	SortModified  SortField = "modified"
	SortRelevance SortField = "relevance"
)

// Query is a normalized search request. Zero values mean "no constraint": an empty
// Text with a Scope lists that subtree; an empty Scope searches every indexed root.
type Query struct {
	Text      string    `json:"text"`
	Scope     string    `json:"scope"`   // restrict to this subtree ("" = all)
	Match     MatchMode `json:"match"`   // defaults to MatchSubstring
	Content   bool      `json:"content"` // search file *contents* (full-text) instead of name/path
	Types     []string  `json:"types"`   // extension filter (lowercased, no dot)
	MinSize   int64     `json:"minSize"` // 0 = no lower bound
	MaxSize   int64     `json:"maxSize"` // 0 = no upper bound
	DirsOnly  bool      `json:"dirsOnly"`
	FilesOnly bool      `json:"filesOnly"`
	Sort      SortField `json:"sort"` // defaults to SortName (or SortRelevance when Text is set)
	Desc      bool      `json:"desc"`
	Limit     int       `json:"limit"`  // defaults to defaultLimit, capped at maxLimit
	Offset    int       `json:"offset"` // opaque pagination cursor (offset-based for the MVP)
}

// Result is one search hit — an Entry plus its relevance score when a text query ran.
type Result struct {
	Entry
	Score float64 `json:"score,omitempty"`
}

// ResultPage is a page of hits plus enough state to fetch the next one.
type ResultPage struct {
	Results    []Result `json:"results"`
	Total      int      `json:"total"`      // total matches (may be an estimate for large sets)
	NextOffset int      `json:"nextOffset"` // -1 when there are no more
	TookMs     int64    `json:"tookMs"`
}

// Status reports index coverage and footprint for the UI's "indexing…" affordances.
type Status struct {
	State          string       `json:"state"` // building | ready | degraded
	FileCount      int64        `json:"fileCount"`
	ContentIndexed int64        `json:"contentIndexed"` // files with full-text indexed (Phase 2)
	ContentPending int64        `json:"contentPending"` // files awaiting a content pass
	ContentBytes   int64        `json:"contentBytes"`   // bytes of indexed text
	ContentBudget  int64        `json:"contentBudget"`  // size budget for indexed text (0 = unlimited)
	DBSizeByte     int64        `json:"dbSizeBytes"`
	Volumes        []VolumeInfo `json:"volumes"`
}

// VolumeInfo is per-root coverage.
type VolumeInfo struct {
	VolumeID     string    `json:"volumeId"`
	Root         string    `json:"root"`
	State        string    `json:"state"`
	LastScan     time.Time `json:"lastScan"`
	IndexedFiles int64     `json:"indexedFiles"`
}
