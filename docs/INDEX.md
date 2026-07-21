# Filesystem Index — Design (RFC)

**Status: Phase 1 (name/path search) is complete** and verified end-to-end in the
browser. The indexer service (`server/indexer/` + the `fw-indexer` binary) does
SQLite FTS5 name/path indexing behind the `Source` interface (portable
walk+fsnotify backend), with **live incremental updates** and an **SSE change
feed**. Core spawns/supervises the child, proxies `/_api/v1/search` +
`/index/status` + `/index/subscribe`, and degrades to 503 when unavailable; the JS
client is `src/search-api.js`. The app builds `fw-indexer` (packaging + dev),
enables it via `FW_INDEX_ROOTS` (explicit-roots policy), and ships a real **Search
panel** (live results, filters, click-to-reveal). **Phase 2 content full-text search
is done** too — a low-priority background scanner fills a contentless FTS5 content
index, with content search in the query and a Name/Contents toggle in the panel
(remaining Phase 2: richer extractors, metadata, size budget). Next: Phase 3 (native
USN/Spotlight backends) and roots-as-preference. Decisions marked **[locked]** are
settled; **[open]** ones need a call before the phase that depends on them.

The index is a searchable, incrementally-maintained catalog of the filesystem,
living in `@files-workbench/core`. Its first consumer is the **Search** activity
(today a placeholder plugin), but the same catalog can back "Recently accessed",
storage/space views, duplicate detection, and fast path completion later.

The core idea: **one normalized interface, many OS-specific backends.** The rest of
the system asks "find files matching X" and gets a uniform result shape; how that
answer is produced — a raw NTFS Master File Table scan, macOS Spotlight, or our own
walk — is hidden behind the interface and chosen per platform.

---

## Goals & non-goals

**v1 goals**
- Instant **name/path search** across indexed roots (the "Everything" experience):
  substring, prefix, glob, scoped to a subtree, with type/size/date filters.
- Correct **incremental maintenance** — the index tracks the filesystem as it
  changes, without periodic full rescans, using each OS's change feed.
- A **persistent on-disk index** that survives restarts, so a relaunch catches up
  from the last change-journal position instead of re-walking. **[locked]**
- **Minimal resource use** — idle-priority I/O, throttled, pausable, size-budgeted.
  The service should be invisible in normal use. **[locked]**

**Designed-for, delivered in a later phase**
- **Content full-text search.** Name/path ships first; content is phase 2. **[locked]**
  We get this from SQLite **FTS5** (BM25 ranking, phrase/prefix/boolean queries) —
  i.e. the "Elasticsearch-like" experience, **locally, with no separate ES/JVM
  service**. The schema is laid out so content is an additive table, not a rewrite.

**Non-goals**
- Running Elasticsearch/OpenSearch or any external search server. It's the wrong
  shape for a single-user desktop app (JVM, separate daemon, ops surface). FTS5
  covers the need; the `Source`/schema abstraction leaves the door open if a true
  distributed backend is ever justified.
- Indexing content of every byte on disk. Content indexing is opt-in-by-heuristic:
  text-like/whitelisted types, under a size cap, within configured roots.
- Replacing `explorer.go`'s live directory listing. The index accelerates *search*;
  directory *browsing* stays a live `readdir` (always current, no index lag).

---

## Process topology **[locked: separate always-on service]**

The indexer is a **standalone long-lived process** (`fw-indexer`), not a goroutine
inside the data server. It owns the on-disk index, runs the walker/watcher, and
answers queries. The Go data server (`server/`) talks to it as a **client**.

```
  ┌────────────┐        ┌──────────────────┐        ┌──────────────────────┐
  │  App (UI)  │ ─────▶ │  core data server │ ─────▶ │   fw-indexer service  │
  │  search    │  HTTP  │  (:8001)          │  local │  • SQLite index       │
  │  plugin    │ ◀───── │  proxies /search  │  RPC   │  • walker + watcher   │
  └────────────┘  SSE   │  under /_api/v1   │ ◀───── │  • per-OS Source impl │
                        └──────────────────┘        └──────────────────────┘
```

Rationale for the layering:
- **The app keeps talking only to core.** Search endpoints appear under the existing
  `/_api/v1/` surface; the app never learns there's a second process. Core forwards
  to the indexer (thin reverse-proxy / embedded client).
- **The OS abstraction lives inside the service** (the `Source` interface below);
  **core holds a thin `Indexer` client interface** — it doesn't care how indexing
  is done, only how to query.
- **Separate process is what "always-on + minimal resources" wants:** the indexer's
  slow background work (walking, content extraction) is isolated from the request
  server's latency, can be scheduled at idle priority independently, and can keep a
  warm index even across quick app restarts.

### Service lifecycle **[open — recommend app-child for MVP]**

"Always-on" has two readings; pick per phase:

- **App-child process (recommended for MVP).** The app (Electron main, the way it
  already spawns the Go server) launches `fw-indexer` at startup and stops it on
  quit. Combined with the persistent index + journal catch-up, the index is
  effectively always warm — a relaunch reconciles in seconds, not a full rescan.
  No OS-service install, no elevation, trivial uninstall.
- **OS-level daemon (later phase).** A real `systemd --user` unit / launchd agent /
  Windows service, running even when the app is closed, for cross-session warmth and
  cross-instance sharing. Bigger surface: install/uninstall, per-OS packaging,
  elevation (esp. Windows MFT), update coordination. Deferred until the app-child
  model proves the feature.

The persistent on-disk index makes the app-child model give ~90% of the daemon's
benefit for a fraction of the platform complexity, so v1 ships app-child.

---

## The OS abstraction

Two interfaces at two layers.

**Inside the service — `Source`** (the per-OS backend; Go's "abstract class"):

```go
// A Source is one OS's window into the filesystem: a way to enumerate it and a
// way to receive changes since a durable cursor. Implementations are selected by
// build tag; the portable one satisfies every platform.
type Source interface {
    // Full enumeration (initial build / forced rescan). Streams entries so the
    // indexer can batch-commit without holding everything in memory.
    Walk(ctx context.Context, root string, emit func(Entry) error) error

    // Incremental changes since `cursor`. Returns a live channel plus the cursor
    // to persist for next start. If the OS lost history past `cursor` (journal
    // wrapped, Spotlight rebuilt), it returns ErrCursorStale and the indexer
    // falls back to Walk.
    Changes(ctx context.Context, cursor Cursor) (<-chan Change, Cursor, error)

    // What this source can do: content? realtime? journal catch-up across restarts?
    Caps() SourceCaps
}
```

Backends (all behind `Source`, added incrementally):
| Platform | v1 (portable) | Native accelerator (later phase) |
|---|---|---|
| **Linux** | walk + `fanotify`/`inotify` watcher + our cursor | — (fragmented; portable is the plan) |
| **Windows** | walk + `ReadDirectoryChangesW` | **MFT enumeration + USN Change Journal** (instant name index, cross-restart catch-up via USN sequence). Pure Go via `golang.org/x/sys/windows` + `DeviceIoControl` — no C#/Python/WASM. Needs an elevated volume handle → the one place a small privileged helper may be warranted. |
| **macOS** | walk + `FSEvents` | **Spotlight** (`NSMetadataQuery`/`mdquery`) — content+metadata already maintained by the OS; FSEvents event-IDs give cross-restart catch-up. |

v1 ships **only the portable `Source`** on all three OSes (walk + native watcher +
our own persisted cursor). That's a complete, shippable index everywhere; the native
accelerators are drop-in optimizations behind the same interface.

**Inside core — `Indexer` client** (thin; no OS knowledge):

```go
type Indexer interface {
    Search(ctx context.Context, q Query) (ResultPage, error)
    Subscribe(ctx context.Context, q Query) (<-chan Delta, error) // live-updating results
    Status(ctx context.Context) (Status, error)                   // coverage, state, on-disk size
    Reindex(ctx context.Context, scope string) error
    Close() error
}
```

---

## Normalized data shapes

The catalog entry and query are the contract the whole app binds to; keep them
OS-agnostic.

```go
type Entry struct {
    Path     string    // absolute, normalized (forward-slash internally)
    Name     string
    Parent   string
    Ext      string
    Size     int64
    Modified time.Time
    Created  time.Time
    IsDir    bool
    VolumeID string    // for per-volume cursors / coverage
    // phase 2:
    // Content  string           // extracted text (indexed, not necessarily stored whole)
    // Meta     map[string]any   // EXIF/ID3/… → JSON column (ties to the details plugin)
}

type Query struct {
    Text    string      // name/path match; content match in phase 2
    Scope   string      // subtree to restrict to ("" = all indexed roots)
    Match   MatchMode   // substring | prefix | glob | fuzzy
    Types   []string    // extension / kind filters
    MinSize, MaxSize int64
    After, Before    time.Time
    Sort    SortSpec    // name | path | size | modified | relevance(BM25)
    Limit   int
    Cursor  string      // opaque pagination cursor
}
```

Change feed pushes `Delta{ Added | Removed | Renamed | Modified, Entry }` so the UI
(reactive stores) live-updates open result sets and the status widget.

---

## Storage — SQLite FTS5 **[locked]**

One embedded DB file under `FW_DATA_DIR` (writable user data — same root the server
already uses). Driver: **`modernc.org/sqlite`** (pure Go, no cgo) so per-OS
cross-compiles stay trivial. *(Note: the server has no SQLite dependency today — the
dir-size cache is an in-memory `sync.Map` — so this is the first DB dependency.)*

Schema sketch:
```sql
-- Name/path index (v1). Trigram FTS5 gives substring matching without full scans.
CREATE TABLE files (
  id INTEGER PRIMARY KEY, volume_id TEXT, parent_id INTEGER,
  name TEXT, path TEXT UNIQUE, ext TEXT,
  size INTEGER, mtime INTEGER, ctime INTEGER, is_dir INTEGER
);
CREATE VIRTUAL TABLE files_fts USING fts5(
  name, path, content='files', content_rowid='id', tokenize='trigram'
);

-- Content index (phase 2) — additive, not a rewrite of the above.
CREATE VIRTUAL TABLE content_fts USING fts5(
  body, content='',                         -- external content; store tokens, not blobs
  tokenize='unicode61 remove_diacritics 2'
);
CREATE TABLE content_meta (file_id INTEGER PRIMARY KEY, indexed_at INTEGER, hash TEXT);

-- Extensible metadata (phase 2): EXIF/ID3/xattrs as JSON (SQLite JSON1 = the JSONB need).
ALTER TABLE files ADD COLUMN meta TEXT;   -- json

-- Per-volume cursor + coverage for journal catch-up and status.
CREATE TABLE volumes (
  volume_id TEXT PRIMARY KEY, root TEXT,
  cursor BLOB,          -- USN sequence / FSEvents eventID / portable walk-generation
  last_full_scan INTEGER, state TEXT   -- building | ready | degraded
);
```

**Size discipline** (they flagged this): name/path index is small (a few hundred
bytes/file). Content is the cost driver, so it's bounded by: whitelist of text-like
types, a per-file size cap, external-content FTS5 (store the tokenized index, not the
original bytes), configurable roots, and an overall on-disk budget with LRU eviction
of least-recently-searched content. Report actual on-disk size via `Status`.

---

## Wire API (service ↔ core ↔ app)

Exposed by the indexer over a **local transport** (localhost HTTP + SSE for the MVP;
a Unix socket / named pipe is a hardening step). Core re-serves them under
`/_api/v1/`:

| Endpoint | Purpose |
|---|---|
| `GET /_api/v1/search?q=&scope=&match=&type=&limit=&cursor=` | one result page |
| `GET /_api/v1/search/subscribe?…` (SSE) | live-updating results / change deltas |
| `GET /_api/v1/index/status` | per-volume coverage, state, on-disk size, queue depth |
| `POST /_api/v1/index/reindex` | force a rescan of a scope |
| `POST /_api/v1/index/roots` | add/remove indexed roots + exclusions |

Reads live on the **data server (:8001)**; there are no mutating index endpoints the
app calls except the explicit control ones, which are administrative. Exclusions
reuse the existing **`FW_BLACKLIST`** rules (`blacklist.go`) so protected/noise paths
are never indexed.

---

## Resource discipline **[locked emphasis]**

Concrete levers so the service stays invisible:
- **Journal catch-up before scanning.** On start, reconcile from the persisted
  `volumes.cursor` (USN/FSEvents/portable-generation). Full `Walk` only on first run
  or `ErrCursorStale`.
- **Idle-priority background scan.** Content indexing runs at low I/O priority
  (`ionice` idle class / Windows background mode), batched commits, bounded queue.
- **Prioritized & pausable.** Index visible/recent directories first; pause under
  battery, high system load, or active user file operations; resume when idle.
- **Debounced watch.** Coalesce bursty change events (e.g. a `git checkout`) before
  touching the DB.
- **Bounded footprint.** Size budget + eviction (above); a memory ceiling on the
  service; WAL mode with periodic checkpoint so the DB file doesn't balloon.

---

## Phasing

- **Phase 0 — this RFC.** Lock the two open items (service lifecycle; whether v1
  indexes content-hash for dedupe now or later).
- **Phase 1 — MVP (portable, all 3 OSes). ✅ DONE.** `fw-indexer` binary; portable
  `Source` (walk + fsnotify watcher); SQLite name/path FTS5; live incremental
  updates + SSE feed; core spawn/supervise + proxy endpoints; the **Search panel**
  UI with live results, filters, and click-to-reveal. *Remaining portable-source
  gaps (deferred to the native backends): no cross-restart journal catch-up — the
  index re-walks its roots on each start (background, idempotent) and doesn't yet
  reconcile deletions that happened while stopped; the persisted SQLite index still
  makes the data survive restarts. Roots are set via `FW_INDEX_ROOTS` (dev defaults
  to `$HOME`, packaged to the user's home) — promoting this to a preference is a
  follow-up.*
- **Phase 2 — content + metadata.** *Content full-text is DONE* (contentless FTS5
  `content_fts` + `content_meta`; a low-priority background scanner that sniffs
  text-vs-binary and indexes text under a per-file size cap, incrementally + on change;
  content search in the query with AND-of-tokens + BM25; a Name/Contents toggle in the
  Search panel). *The total size budget is DONE too* — `content_meta.body_bytes` tracks
  indexed-text bytes; once `FW_INDEX_CONTENT_BUDGET` (default 512 MiB) is reached the
  scanner stops indexing NEW files (a changed already-indexed file still re-indexes),
  bounding the content index's footprint; `/status` reports `contentBytes`/`contentBudget`.
  *Document extractors are DONE* — `extractText` dispatches by type: UTF-8 text/code
  directly, `.docx` via the stdlib zip/XML, and `.pdf` via the external `pdftotext`
  (poppler-utils — optional; PDFs are simply not content-indexed when it's absent),
  each with a source-size cap + a 20s subprocess timeout. Remaining Phase 2: more
  types (xlsx/pptx/odt/epub), a JSON metadata column (EXIF/ID3), and LRU eviction
  (today the budget is stop-when-full — freed budget doesn't backfill skipped files
  until they change).
- **Phase 3 — native accelerators.** Windows USN/MFT and macOS Spotlight/FSEvents
  behind the existing `Source` interface. Faster first-index and cross-restart
  catch-up; no API change.
- **Phase 4 (optional) — OS-level daemon.** Promote the app-child service to a real
  user daemon for cross-session warmth, if the feature earns it.

---

## Open questions

1. **Service lifecycle** — app-child (recommended MVP) vs OS-level daemon. *(above)*
2. **Content-hash in v1?** Storing a content hash per file enables duplicate
   detection and cheap "unchanged since last index" checks, but costs a full read
   per file. Include the column in the schema now; populate it lazily in phase 2.
3. **Roots policy** — index the user's home + mounted drives by default, or only
   explicitly added roots? (Affects first-run cost and privacy.)
4. **Transport hardening** — localhost HTTP is fine for the MVP; move to a Unix
   socket / named pipe (no TCP port, OS-enforced access) before content indexing
   makes the index sensitive.
