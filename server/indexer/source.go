package indexer

import (
	"context"
	"encoding/json"
)

// Source is one OS's window into the filesystem — the abstraction the whole design
// turns on (docs/INDEX.md). A Source can enumerate a tree and stream live changes;
// the indexer service drives it without knowing which OS mechanism is underneath.
//
// v1 ships one implementation, portableSource (filepath.WalkDir + fsnotify), which
// works everywhere. Native accelerators — Windows MFT/USN, macOS Spotlight/FSEvents
// — implement the same interface later and are swapped in per platform, with no
// change to the service or the store.
type Source interface {
	// Walk enumerates root, calling emit for each entry. Used for the initial index
	// and forced rescans. Returns the count emitted.
	Walk(ctx context.Context, root string, emit func(Entry) error) (int64, error)

	// Watch streams live changes for the given roots until ctx is cancelled, then
	// closes the channel. The portable source uses fsnotify; native backends tail
	// the OS change journal (and can resume across restarts — see Caps).
	Watch(ctx context.Context, roots []string) (<-chan Change, error)

	// Caps reports what this backend can do, so the service can adapt (e.g. schedule
	// periodic reconciles when Realtime is false).
	Caps() SourceCaps
}

// SourceCaps advertises a backend's abilities.
type SourceCaps struct {
	Realtime       bool // emits live changes via Watch
	Content        bool // supplies file content for full-text search (phase 2)
	JournalCatchup bool // can resume from a durable cursor across restarts (native backends)
}

// ChangeOp is the kind of a live filesystem change.
type ChangeOp int

const (
	ChangeAdded ChangeOp = iota
	ChangeModified
	ChangeRemoved
)

func (o ChangeOp) String() string {
	switch o {
	case ChangeAdded:
		return "added"
	case ChangeModified:
		return "modified"
	case ChangeRemoved:
		return "removed"
	default:
		return "unknown"
	}
}

func (o ChangeOp) MarshalJSON() ([]byte, error) { return json.Marshal(o.String()) }

// Change is one live filesystem event the index applies and fans out to subscribers.
// For ChangeRemoved only Entry.Path is guaranteed (the object is already gone).
type Change struct {
	Op    ChangeOp `json:"op"`
	Entry Entry    `json:"entry"`
}
