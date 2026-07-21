package indexer

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the on-disk index — a SQLite database (pure-Go driver, no cgo, so per-OS
// cross-compiles stay trivial). It owns the schema and every read/write against it.
//
// The name/path index is `files` + an external-content FTS5 table with the trigram
// tokenizer (substring matching, kept in sync by triggers). Content full-text
// (docs/INDEX.md phase 2) will be an additive `content_fts` table, not a rewrite.
type Store struct {
	db   *sql.DB
	path string
}

const (
	defaultLimit = 200
	maxLimit     = 2000
	batchSize    = 5000 // rows per transaction during a bulk index — bounds WAL growth
)

// Open opens (creating if absent) the index at dbPath and ensures the schema.
// dbPath may be ":memory:" for tests.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	// A single writer with WAL: readers never block the background indexer's writes.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	s := &Store{db: db, path: dbPath}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS files (
			id        INTEGER PRIMARY KEY,
			volume_id TEXT    NOT NULL DEFAULT '',
			parent_id INTEGER,                       -- populated by a later phase
			name      TEXT    NOT NULL,
			path      TEXT    NOT NULL UNIQUE,
			ext       TEXT    NOT NULL DEFAULT '',
			size      INTEGER NOT NULL DEFAULT 0,
			mtime     INTEGER NOT NULL DEFAULT 0,    -- unix seconds
			ctime     INTEGER NOT NULL DEFAULT 0,    -- best-effort until a native backend supplies birth time
			is_dir    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS files_volume ON files(volume_id)`,
		`CREATE INDEX IF NOT EXISTS files_ext    ON files(ext)`,
		// External-content FTS5: the index references files rows (content='files'),
		// so we don't store name/path twice. Trigram = substring matching.
		`CREATE VIRTUAL TABLE IF NOT EXISTS files_fts USING fts5(
			name, path, content='files', content_rowid='id', tokenize='trigram'
		)`,
		// Triggers keep the FTS index in lockstep with files (incl. UPSERT's UPDATE).
		`CREATE TRIGGER IF NOT EXISTS files_ai AFTER INSERT ON files BEGIN
			INSERT INTO files_fts(rowid, name, path) VALUES (new.id, new.name, new.path);
		END`,
		`CREATE TRIGGER IF NOT EXISTS files_ad AFTER DELETE ON files BEGIN
			INSERT INTO files_fts(files_fts, rowid, name, path) VALUES ('delete', old.id, old.name, old.path);
		END`,
		`CREATE TRIGGER IF NOT EXISTS files_au AFTER UPDATE ON files BEGIN
			INSERT INTO files_fts(files_fts, rowid, name, path) VALUES ('delete', old.id, old.name, old.path);
			INSERT INTO files_fts(rowid, name, path) VALUES (new.id, new.name, new.path);
		END`,
		`CREATE TABLE IF NOT EXISTS volumes (
			volume_id      TEXT PRIMARY KEY,
			root           TEXT NOT NULL,
			cursor         BLOB,                  -- USN seq / FSEvents id / walk generation (unused until the watcher lands)
			last_scan      INTEGER NOT NULL DEFAULT 0,
			state          TEXT    NOT NULL DEFAULT 'building'
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w\n%s", err, stmt)
		}
	}
	return nil
}

const upsertSQL = `
INSERT INTO files (volume_id, name, path, ext, size, mtime, ctime, is_dir)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
	volume_id=excluded.volume_id, name=excluded.name, ext=excluded.ext,
	size=excluded.size, mtime=excluded.mtime, ctime=excluded.ctime, is_dir=excluded.is_dir`

// Upsert inserts or updates a single entry. For bulk indexing use a Batch.
func (s *Store) Upsert(e Entry) error {
	_, err := s.db.Exec(upsertSQL, e.VolumeID, e.Name, normPath(e.Path), e.Ext,
		e.Size, unix(e.Modified), unix(e.Created), boolToInt(e.IsDir))
	return err
}

// DeleteSubtree removes an entry and everything beneath it (a directory removal).
// Returns the number of rows deleted.
func (s *Store) DeleteSubtree(path string) (int64, error) {
	p := normPath(path)
	res, err := s.db.Exec(
		`DELETE FROM files WHERE path = ? OR path LIKE ? ESCAPE '\'`,
		p, likePrefix(p)+"/%")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Batch bulk-inserts entries in bounded transactions (batchSize rows each) so a
// full walk of a large tree never holds one enormous transaction open.
type Batch struct {
	store *Store
	tx    *sql.Tx
	stmt  *sql.Stmt
	n     int
}

func (s *Store) NewBatch() (*Batch, error) {
	b := &Batch{store: s}
	if err := b.begin(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Batch) begin() error {
	tx, err := b.store.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(upsertSQL)
	if err != nil {
		tx.Rollback()
		return err
	}
	b.tx, b.stmt, b.n = tx, stmt, 0
	return nil
}

// Add queues one entry, flushing the current transaction automatically every
// batchSize rows.
func (b *Batch) Add(e Entry) error {
	if _, err := b.stmt.Exec(e.VolumeID, e.Name, normPath(e.Path), e.Ext,
		e.Size, unix(e.Modified), unix(e.Created), boolToInt(e.IsDir)); err != nil {
		return err
	}
	b.n++
	if b.n >= batchSize {
		if err := b.flush(); err != nil {
			return err
		}
		return b.begin()
	}
	return nil
}

func (b *Batch) flush() error {
	b.stmt.Close()
	return b.tx.Commit()
}

// Commit flushes any queued rows. The Batch must not be used afterward.
func (b *Batch) Commit() error { return b.flush() }

// unix/boolToInt/normPath/likePrefix are small storage helpers.
func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// normPath stores paths with forward slashes so the index is comparable across OSes.
func normPath(p string) string { return strings.ReplaceAll(p, `\`, `/`) }

// likePrefix escapes LIKE wildcards in a literal path prefix (ESCAPE '\').
func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p)
}

// fileCount is the total number of indexed entries.
func (s *Store) fileCount() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&n)
	return n, err
}

// recordVolume upserts a root's coverage row (last_scan + state).
func (s *Store) recordVolume(vid, root, state string) error {
	_, err := s.db.Exec(
		`INSERT INTO volumes (volume_id, root, last_scan, state) VALUES (?, ?, ?, ?)
		 ON CONFLICT(volume_id) DO UPDATE SET root=excluded.root, last_scan=excluded.last_scan, state=excluded.state`,
		vid, root, time.Now().Unix(), state)
	return err
}

// dbSizeBytes reports the on-disk footprint. In WAL mode uncheckpointed data lives
// in the -wal sidecar, so a main-file-only stat under-reports badly mid-index —
// sum the main file, the WAL, and the shared-memory index.
func (s *Store) dbSizeBytes() int64 {
	if s.path == ":memory:" {
		return 0
	}
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}
