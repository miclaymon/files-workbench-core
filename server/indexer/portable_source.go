package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// portableSource is the cross-platform Source: filepath.WalkDir for enumeration and
// fsnotify for live changes. It works on every OS and is what v1 ships. Native
// backends (USN/MFT, Spotlight) implement Source too and replace it per platform.
//
// Watch limitation (inherent to the portable approach): fsnotify watches directories
// non-recursively, so we add a watch per directory and manage them as dirs come and
// go. Very large trees can exhaust the OS watch budget (Linux
// fs.inotify.max_user_watches); that's the motivation for the native fanotify/USN
// backends — here we log and degrade rather than fail.
type portableSource struct {
	exclude ExcludeFunc
	log     func(string)
}

// NewPortableSource returns the cross-platform Source (walk + fsnotify). log is an
// optional sink for degradation notices (watch-budget exhaustion, etc.).
func NewPortableSource(exclude ExcludeFunc, log func(string)) Source {
	if exclude == nil {
		exclude = func(string, bool) bool { return false }
	}
	if log == nil {
		log = func(string) {}
	}
	return &portableSource{exclude: exclude, log: log}
}

func (p *portableSource) Caps() SourceCaps {
	return SourceCaps{Realtime: true, Content: false, JournalCatchup: false}
}

func (p *portableSource) Walk(ctx context.Context, root string, emit func(Entry) error) (int64, error) {
	// Thread cancellation through the walk by failing emit once ctx is done.
	return Walk(root, p.exclude, func(e Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(e)
	})
}

func (p *portableSource) Watch(ctx context.Context, roots []string) (<-chan Change, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// Absolute, normalized roots — used to attribute a changed path to its volume.
	absRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		if a, err := filepath.Abs(r); err == nil {
			absRoots = append(absRoots, a)
		}
	}

	watch := &portableWatch{src: p, w: w, roots: absRoots, watched: map[string]bool{}}
	// Seed watches for the existing tree. (Changes between the initial Walk and now
	// can be missed — a known portable-source gap the native journals close.)
	for _, r := range absRoots {
		watch.addTree(r)
	}

	out := make(chan Change, 256)
	go watch.run(ctx, out)
	return out, nil
}

// portableWatch is the running state of one Watch call.
type portableWatch struct {
	src     *portableSource
	w       *fsnotify.Watcher
	roots   []string
	mu      sync.Mutex
	watched map[string]bool
}

// addTree adds a watch for dir and every (non-excluded) directory beneath it.
func (pw *portableWatch) addTree(dir string) {
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if pw.src.exclude(path, true) {
			return filepath.SkipDir
		}
		pw.addWatch(path)
		return nil
	})
}

func (pw *portableWatch) addWatch(dir string) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.watched[dir] {
		return
	}
	if err := pw.w.Add(dir); err != nil {
		// Most likely the OS watch budget is exhausted; degrade rather than abort.
		pw.src.log("watch add failed for " + dir + ": " + err.Error())
		return
	}
	pw.watched[dir] = true
}

func (pw *portableWatch) run(ctx context.Context, out chan<- Change) {
	defer close(out)
	defer pw.w.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-pw.w.Events:
			if !ok {
				return
			}
			pw.handle(ctx, ev, out)
		case err, ok := <-pw.w.Errors:
			if !ok {
				return
			}
			pw.src.log("watch error: " + err.Error())
		}
	}
}

func (pw *portableWatch) handle(ctx context.Context, ev fsnotify.Event, out chan<- Change) {
	switch {
	case ev.Has(fsnotify.Create):
		info, err := os.Lstat(ev.Name)
		if err != nil {
			return // created then immediately gone
		}
		if pw.src.exclude(ev.Name, info.IsDir()) {
			return
		}
		if info.IsDir() {
			// A new subtree: watch it, and walk it so pre-existing children (created
			// before we could add the watch) are indexed too.
			pw.addTree(ev.Name)
			Walk(ev.Name, pw.src.exclude, func(e Entry) error {
				return send(ctx, out, Change{Op: ChangeAdded, Entry: e})
			})
			return
		}
		send(ctx, out, Change{Op: ChangeAdded, Entry: entryFromInfo(ev.Name, pw.volumeFor(ev.Name), info)})

	case ev.Has(fsnotify.Write):
		info, err := os.Lstat(ev.Name)
		if err != nil || info.IsDir() {
			return // a dir "write" is a child change, already covered by the child event
		}
		send(ctx, out, Change{Op: ChangeModified, Entry: entryFromInfo(ev.Name, pw.volumeFor(ev.Name), info)})

	case ev.Has(fsnotify.Remove), ev.Has(fsnotify.Rename):
		// The path is gone (rename fires on the old name; the new name arrives as a
		// Create). Drop any watch on it; the store deletes it and any subtree.
		pw.dropWatch(ev.Name)
		send(ctx, out, Change{Op: ChangeRemoved, Entry: Entry{Path: ev.Name}})
	}
}

func (pw *portableWatch) dropWatch(dir string) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.watched[dir] {
		pw.w.Remove(dir)
		delete(pw.watched, dir)
	}
}

// volumeFor attributes a changed path to the watched root it lives under.
func (pw *portableWatch) volumeFor(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	for _, r := range pw.roots {
		if abs == r || strings.HasPrefix(abs, r+string(filepath.Separator)) {
			return normPath(r)
		}
	}
	return ""
}

// send delivers a change unless ctx is cancelled (so a full buffer can't wedge the
// watcher on shutdown).
func send(ctx context.Context, out chan<- Change, c Change) error {
	select {
	case out <- c:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
