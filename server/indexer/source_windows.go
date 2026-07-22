//go:build windows

package indexer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// windowsSource is the Windows Source (Phase 3). ⚠️ RUNTIME-UNTESTED — see the header
// of usn_windows.go. It uses the portable recursive walk for the initial index
// (correct on Windows; MFT-enum acceleration is a further optimization) and the NTFS
// USN change journal for live updates with cross-restart catch-up. Any journal setup
// failure (no elevation, non-NTFS volume) falls back to the portable fsnotify watcher,
// so the indexer keeps working regardless.
type windowsSource struct {
	exclude  ExcludeFunc
	log      func(string)
	fallback Source
	cursors  *usnCursorStore
}

func newWindowsSource(exclude ExcludeFunc, log func(string)) Source {
	return &windowsSource{
		exclude:  exclude,
		log:      log,
		fallback: NewPortableSource(exclude, log),
		cursors:  newUsnCursorStore(),
	}
}

func (w *windowsSource) Caps() SourceCaps {
	return SourceCaps{Realtime: true, Content: false, JournalCatchup: true}
}

// Walk uses the portable recursive walk (MFT enumeration is a future optimization).
func (w *windowsSource) Walk(ctx context.Context, root string, emit func(Entry) error) (int64, error) {
	return w.fallback.Walk(ctx, root, emit)
}

// Watch tails the USN journal of each volume the roots live on, catching up from a
// persisted cursor first. If no volume's journal can be opened, it falls back to the
// portable watcher.
func (w *windowsSource) Watch(ctx context.Context, roots []string) (<-chan Change, error) {
	byVolume := map[string][]string{}
	for _, r := range roots {
		if abs, err := filepath.Abs(r); err == nil {
			byVolume[volumeLetter(abs)] = append(byVolume[volumeLetter(abs)], normPath(abs))
		}
	}

	out := make(chan Change, 256)
	var wg sync.WaitGroup
	started := 0
	for letter, vroots := range byVolume {
		h, err := openVolume(letter)
		if err != nil {
			w.log("usn: cannot open volume " + letter + ": " + err.Error())
			continue
		}
		jd, err := queryJournal(h)
		if err != nil {
			w.log("usn: no journal on " + letter + ": " + err.Error())
			windows.CloseHandle(h)
			continue
		}
		started++
		wg.Add(1)
		go func(letter string, h windows.Handle, jd usnJournalData, vroots []string) {
			defer wg.Done()
			defer windows.CloseHandle(h)
			w.tailVolume(ctx, letter, h, jd, vroots, out)
		}(letter, h, jd, vroots)
	}

	if started == 0 {
		// No usable journal (unelevated / non-NTFS) — degrade to the portable watcher.
		w.log("usn: no volume journals available, using portable watcher")
		return w.fallback.Watch(ctx, roots)
	}
	go func() { wg.Wait(); close(out) }()
	return out, nil
}

// tailVolume catches up from the persisted cursor then polls the journal for a volume,
// emitting Changes for records under vroots.
func (w *windowsSource) tailVolume(ctx context.Context, letter string, h windows.Handle, jd usnJournalData, vroots []string, out chan<- Change) {
	start := jd.NextUsn
	if cur, ok := w.cursors.get(letter); ok && cur.JournalID == jd.UsnJournalID && cur.Usn >= jd.LowestValidUsn && cur.Usn <= jd.NextUsn {
		start = cur.Usn // resume from where we left off (cross-restart catch-up)
	}
	pathCache := map[uint64]string{}
	for {
		if ctx.Err() != nil {
			return
		}
		recs, next, err := readJournal(h, jd.UsnJournalID, start)
		if err != nil {
			w.log("usn read " + letter + ": " + err.Error())
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, rec := range recs {
			if c, ok := w.recordToChange(h, letter, rec, vroots, pathCache); ok {
				if send(ctx, out, c) != nil {
					return
				}
			}
		}
		w.cursors.set(letter, usnCursor{JournalID: jd.UsnJournalID, Usn: next})
		if next == start {
			// Caught up — wait before polling again (READ_USN_JOURNAL can block with a
			// timeout, but a simple poll keeps this backend self-contained).
			if !sleep(ctx, time.Second) {
				return
			}
		}
		start = next
	}
}

// recordToChange maps a USN record to a Change, resolving its path and filtering to
// the watched roots + exclusions.
func (w *windowsSource) recordToChange(h windows.Handle, letter string, rec usnRecord, vroots []string, cache map[uint64]string) (Change, bool) {
	parent, ok := cache[rec.ParentFileRefNumber]
	if !ok {
		p, err := resolveByID(h, rec.ParentFileRefNumber)
		if err != nil {
			return Change{}, false // parent gone/unresolvable — skip
		}
		parent = normPath(p)
		cache[rec.ParentFileRefNumber] = parent
	}
	path := parent + "/" + rec.FileName
	if !underAny(path, vroots) || w.exclude(path, rec.isDir()) {
		return Change{}, false
	}

	switch {
	case rec.Reason&(usnReasonFileDelete|usnReasonRenameOldName) != 0:
		return Change{Op: ChangeRemoved, Entry: Entry{Path: path}}, true
	case rec.Reason&(usnReasonFileCreate|usnReasonRenameNewName) != 0:
		if e, ok := statEntry(path, normPath(mountFor(letter))); ok {
			return Change{Op: ChangeAdded, Entry: e}, true
		}
	case rec.Reason&(usnReasonDataOverwrite|usnReasonDataExtend|usnReasonDataTrunc) != 0:
		if e, ok := statEntry(path, normPath(mountFor(letter))); ok {
			return Change{Op: ChangeModified, Entry: e}, true
		}
	}
	return Change{}, false
}

// volumeLetter returns the drive letter of an absolute Windows path ("C:\x" → "C").
func volumeLetter(abs string) string {
	v := filepath.VolumeName(abs) // "C:"
	return strings.TrimSuffix(v, ":")
}

func mountFor(letter string) string { return letter + ":/" }

// ── Cursor persistence (cross-restart catch-up) ───────────────────────────────

type usnCursor struct {
	JournalID uint64 `json:"journalId"`
	Usn       int64  `json:"usn"`
}

// usnCursorStore persists per-volume USN cursors to <FW_DATA_DIR>/index/usn-cursors.json
// so a relaunch resumes reading the journal where it stopped instead of re-walking.
type usnCursorStore struct {
	mu   sync.Mutex
	path string
	data map[string]usnCursor
}

func newUsnCursorStore() *usnCursorStore {
	dir := os.Getenv("FW_DATA_DIR")
	if dir == "" {
		dir = ".fw"
	}
	s := &usnCursorStore{
		path: filepath.Join(dir, "index", "usn-cursors.json"),
		data: map[string]usnCursor{},
	}
	if b, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(b, &s.data)
	}
	return s
}

func (s *usnCursorStore) get(letter string) (usnCursor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.data[letter]
	return c, ok
}

func (s *usnCursorStore) set(letter string, c usnCursor) {
	s.mu.Lock()
	s.data[letter] = c
	b, _ := json.Marshal(s.data)
	s.mu.Unlock()
	if len(b) > 0 {
		os.MkdirAll(filepath.Dir(s.path), 0o755)
		os.WriteFile(s.path, b, 0o644)
	}
}
