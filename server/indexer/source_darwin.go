//go:build darwin

package indexer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// darwinSource is the macOS Source (Phase 3). ⚠️ WRITTEN, NOT WIRED, AND — unlike
// the Windows backend — NOT EVEN BUILD-VERIFIED. See fsevents_darwin.go's header
// and docs/MACOS_BACKEND_PLAN.md for the full status and a step-by-step plan to
// finish, debug, and wire it up on a real Mac. SelectSource
// (source_select_darwin.go) does not call newDarwinSource today; this type exists
// so most of the implementation is ready when someone picks this up.
type darwinSource struct {
	exclude  ExcludeFunc
	log      func(string)
	fallback Source
	cursors  *fsEventsCursorStore
}

func newDarwinSource(exclude ExcludeFunc, log func(string)) Source {
	return &darwinSource{
		exclude:  exclude,
		log:      log,
		fallback: NewPortableSource(exclude, log),
		cursors:  newFSEventsCursorStore(),
	}
}

func (d *darwinSource) Caps() SourceCaps {
	return SourceCaps{Realtime: true, Content: false, JournalCatchup: true}
}

// Walk uses the portable recursive walk. macOS has no public bulk-enumeration API
// analogous to Windows' MFT (Spotlight's own catalog is the closest thing, but
// querying "everything" through NSMetadataQuery isn't obviously faster or more
// complete than a walk, and it would add a Spotlight dependency to the *initial*
// index too, not just the live-update path) — see docs/MACOS_BACKEND_PLAN.md,
// "Initial index", for the tradeoff.
func (d *darwinSource) Walk(ctx context.Context, root string, emit func(Entry) error) (int64, error) {
	return d.fallback.Walk(ctx, root, emit)
}

// Watch opens one FSEvents stream covering all roots — unlike Windows, FSEvents
// takes an arbitrary path list directly, so there's no per-volume journal grouping
// to do here — resuming from a persisted event ID when every root's volume still
// has the FSEvents log identity (UUID) it had when the cursor was saved. Falls back
// to the portable fsnotify watcher on any stream-creation failure.
func (d *darwinSource) Watch(ctx context.Context, roots []string) (<-chan Change, error) {
	absRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		if a, err := filepath.Abs(r); err == nil {
			absRoots = append(absRoots, normPath(a))
		}
	}
	if len(absRoots) == 0 {
		return d.fallback.Watch(ctx, roots)
	}

	since, deviceUUIDs, resuming := d.resumeSince(absRoots)
	if resuming {
		d.log("fsevents: resuming from persisted event id")
	} else {
		d.log("fsevents: starting fresh (no valid cursor) — no history replay")
	}

	out := make(chan Change, 256)
	w := &darwinWatch{src: d, roots: absRoots, out: out, deviceUUIDs: deviceUUIDs}
	stream, err := newFSEventStream(absRoots, since, 300*time.Millisecond, w)
	if err != nil {
		d.log("fsevents: stream create failed, using portable watcher: " + err.Error())
		return d.fallback.Watch(ctx, roots)
	}
	w.stream = stream

	go w.run(ctx)
	return out, nil
}

// resumeSince decides the FSEventStreamCreate `since` value: the persisted event ID
// if every root's volume UUID still matches what was recorded when it was saved
// (proving that volume's event log hasn't been reset since), otherwise
// fsEventsSinceNow. deviceUUIDs is the current per-root UUID snapshot, threaded
// through to darwinWatch so it can be re-persisted alongside future cursor updates.
func (d *darwinSource) resumeSince(roots []string) (since uint64, deviceUUIDs map[string]string, resuming bool) {
	cur := d.cursors.get()
	deviceUUIDs = map[string]string{}
	allMatch := cur.LatestEventID != 0 && len(cur.DeviceUUIDs) > 0
	for _, r := range roots {
		dev, ok := deviceOf(r)
		if !ok {
			allMatch = false
			continue
		}
		key := strconv.FormatUint(dev, 10)
		uuid, ok := fsEventsUUIDForDevice(dev)
		if !ok {
			// Not FSEvents-eligible (e.g. a network volume) — can't validate a resume,
			// so don't attempt one.
			allMatch = false
			continue
		}
		deviceUUIDs[key] = uuid
		if prev, seen := cur.DeviceUUIDs[key]; !seen || prev != uuid {
			allMatch = false
		}
	}
	if allMatch {
		return cur.LatestEventID, deviceUUIDs, true
	}
	return fsEventsSinceNow, deviceUUIDs, false
}

// deviceOf returns the dev_t (as reported in syscall.Stat_t.Dev) of the volume
// containing path.
func deviceOf(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

// darwinWatch is the running state of one Watch call — the fsEventSink that
// receives raw batches from fsevents_darwin.go and turns them into Changes.
type darwinWatch struct {
	src         *darwinSource
	roots       []string
	out         chan<- Change
	stream      *fsEventStream
	deviceUUIDs map[string]string
}

func (w *darwinWatch) run(ctx context.Context) {
	<-ctx.Done()
	w.stream.Close()
	close(w.out)
}

// onBatch implements fsEventSink. It's invoked from the C callback (see
// goFSEventsCallback in fsevents_darwin.go) on FSEvents' own dispatch-queue thread,
// so it must not block for long — emit() is a non-blocking, drop-and-log send for
// exactly that reason (see its comment). Persists the cursor once per batch using
// the highest event ID seen, after processing every event in it.
func (w *darwinWatch) onBatch(events []fsEvent) {
	var last uint64
	for _, ev := range events {
		switch {
		case ev.Flags&flagHistoryDone != 0:
			w.src.log("fsevents: caught up to live events")
		case ev.Flags&flagRootChanged != 0:
			w.src.log("fsevents: watched root changed/removed: " + ev.Path)
		case ev.Flags&(flagMustScanSubDirs|flagUserDropped|flagKernelDropped) != 0:
			// FSEvents coalesced or dropped granular info under this path (or, if Path
			// is empty, possibly volume-wide) — the only correct recovery is a rescan.
			// This emits Added/Modified for everything currently there; it does NOT
			// detect deletions that happened during the gap (Walk only sees what still
			// exists). A full reconciliation would need to diff against the store's
			// existing rows under that path — the Source layer has no store access, so
			// that's a service-level concern if this gap matters in practice. See
			// docs/MACOS_BACKEND_PLAN.md, "Known gaps".
			w.rescan(ev.Path)
		default:
			w.handleEvent(ev.Path, ev.Flags)
		}
		if ev.ID > last {
			last = ev.ID
		}
	}
	if last > 0 {
		w.src.cursors.set(last, w.deviceUUIDs)
	}
}

// handleEvent maps one per-item FSEvents record to a Change. FSEvents' Created/
// Renamed/Modified flags are advisory (in particular, ItemRenamed fires for both
// the old and new path as separate records, with no way to tell which from the
// flags alone) — so, matching the Windows and portable backends, the path is always
// re-stated: if it still exists that's Added-or-Modified, otherwise Removed.
func (w *darwinWatch) handleEvent(path string, flags uint32) {
	abs := normPath(filepath.Clean(path))
	if !underAny(abs, w.roots) {
		return
	}
	isDirHint := flags&flagItemIsDir != 0
	if w.src.exclude(abs, isDirHint) {
		return
	}
	if e, ok := statEntry(abs, w.volumeFor(abs)); ok {
		op := ChangeModified
		if flags&flagItemCreated != 0 {
			op = ChangeAdded
		}
		w.emit(Change{Op: op, Entry: e})
		return
	}
	w.emit(Change{Op: ChangeRemoved, Entry: Entry{Path: abs}})
}

// rescan re-walks path (or, if path is empty/out of scope — a volume-wide signal —
// every watched root) after a MustScanSubDirs/UserDropped/KernelDropped flag.
func (w *darwinWatch) rescan(path string) {
	abs := normPath(filepath.Clean(path))
	if path == "" || !underAny(abs, w.roots) {
		for _, r := range w.roots {
			w.rescanRoot(r)
		}
		return
	}
	w.rescanRoot(abs)
}

func (w *darwinWatch) rescanRoot(root string) {
	w.src.log("fsevents: rescanning " + root + " (coalesced/dropped events)")
	Walk(root, w.src.exclude, func(e Entry) error {
		w.emit(Change{Op: ChangeModified, Entry: e})
		return nil
	})
}

// volumeFor attributes a changed path to the watched root it lives under.
func (w *darwinWatch) volumeFor(path string) string {
	for _, r := range w.roots {
		if path == r || len(path) > len(r) && path[:len(r)+1] == r+"/" {
			return r
		}
	}
	return ""
}

// emit is a non-blocking send: onBatch runs on FSEvents' dispatch-queue thread, and
// blocking it risks stalling event delivery if the consumer falls behind. Dropping
// and logging is the safe default for an unverified first draft; if drops prove
// costly in real testing, consider decoupling with an internal unbounded queue
// drained by a separate goroutine instead of tying delivery directly to consumption
// speed. See docs/MACOS_BACKEND_PLAN.md, "Known gaps".
func (w *darwinWatch) emit(c Change) {
	select {
	case w.out <- c:
	default:
		w.src.log("fsevents: change channel full, dropping event for " + c.Entry.Path)
	}
}

// ── Cursor persistence (cross-restart catch-up) ───────────────────────────────

// fsEventsCursor is the persisted resume point: the last event ID processed, plus
// the per-volume FSEvents UUID it was valid for (see resumeSince/deviceUUIDs).
type fsEventsCursor struct {
	LatestEventID uint64            `json:"latestEventId"`
	DeviceUUIDs   map[string]string `json:"deviceUuids"`
}

// fsEventsCursorStore persists the cursor to
// <FW_DATA_DIR>/index/fsevents-cursor.json — mirrors usnCursorStore in
// source_windows.go.
type fsEventsCursorStore struct {
	mu   sync.Mutex
	path string
	data fsEventsCursor
}

func newFSEventsCursorStore() *fsEventsCursorStore {
	dir := os.Getenv("FW_DATA_DIR")
	if dir == "" {
		dir = ".fw"
	}
	s := &fsEventsCursorStore{
		path: filepath.Join(dir, "index", "fsevents-cursor.json"),
		data: fsEventsCursor{DeviceUUIDs: map[string]string{}},
	}
	if b, err := os.ReadFile(s.path); err == nil {
		json.Unmarshal(b, &s.data)
	}
	if s.data.DeviceUUIDs == nil {
		s.data.DeviceUUIDs = map[string]string{}
	}
	return s
}

func (s *fsEventsCursorStore) get() fsEventsCursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	dup := make(map[string]string, len(s.data.DeviceUUIDs))
	for k, v := range s.data.DeviceUUIDs {
		dup[k] = v
	}
	return fsEventsCursor{LatestEventID: s.data.LatestEventID, DeviceUUIDs: dup}
}

func (s *fsEventsCursorStore) set(eventID uint64, deviceUUIDs map[string]string) {
	s.mu.Lock()
	s.data.LatestEventID = eventID
	for k, v := range deviceUUIDs {
		s.data.DeviceUUIDs[k] = v
	}
	b, _ := json.Marshal(s.data)
	s.mu.Unlock()
	if len(b) > 0 {
		os.MkdirAll(filepath.Dir(s.path), 0o755)
		os.WriteFile(s.path, b, 0o644)
	}
}
