package indexer

import (
	"database/sql"
	"time"
)

// contentCandidate is a file the content scanner may need to (re)index: identified
// by its files.id, with the current mtime/size to compare against content_meta.
type contentCandidate struct {
	ID    int64
	Path  string
	MTime int64
	Size  int64
}

// FilesNeedingContent returns up to limit files whose content hasn't been examined
// at their current mtime (new files, or files modified since their last content
// pass). Directories are excluded. Ordered by id so repeated calls make progress.
func (s *Store) FilesNeedingContent(limit int) ([]contentCandidate, error) {
	rows, err := s.db.Query(`
		SELECT f.id, f.path, f.mtime, f.size
		FROM files f
		LEFT JOIN content_meta cm ON cm.file_id = f.id
		WHERE f.is_dir = 0 AND (cm.file_id IS NULL OR cm.mtime != f.mtime)
		ORDER BY f.id
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contentCandidate
	for rows.Next() {
		var c contentCandidate
		if err := rows.Scan(&c.ID, &c.Path, &c.MTime, &c.Size); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// IndexContent stores a file's extracted text in the content index and marks it
// examined+indexed. Contentless FTS5 has no in-place update, so replace = delete +
// insert by rowid (= file id).
func (s *Store) IndexContent(fileID int64, body string, mtime, size int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM content_fts WHERE rowid = ?`, fileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO content_fts(rowid, body) VALUES (?, ?)`, fileID, body); err != nil {
		return err
	}
	if err := upsertContentMeta(tx, fileID, mtime, size, true); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkContentSkipped records that a file was examined at this mtime but not indexed
// (binary, too large, unreadable) so the scanner won't reconsider it until it changes.
func (s *Store) MarkContentSkipped(fileID, mtime, size int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Drop any stale content from a previous version that WAS indexed.
	if _, err := tx.Exec(`DELETE FROM content_fts WHERE rowid = ?`, fileID); err != nil {
		return err
	}
	if err := upsertContentMeta(tx, fileID, mtime, size, false); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertContentMeta(tx *sql.Tx, fileID, mtime, size int64, indexed bool) error {
	_, err := tx.Exec(`
		INSERT INTO content_meta(file_id, mtime, size, indexed, indexed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(file_id) DO UPDATE SET
			mtime=excluded.mtime, size=excluded.size, indexed=excluded.indexed, indexed_at=excluded.indexed_at`,
		fileID, mtime, size, boolToInt(indexed), time.Now().Unix())
	return err
}

// contentStats reports how many files have content indexed (for /status).
func (s *Store) contentStats() (indexed int64, examined int64) {
	s.db.QueryRow(`SELECT COUNT(*) FROM content_meta WHERE indexed = 1`).Scan(&indexed)
	s.db.QueryRow(`SELECT COUNT(*) FROM content_meta`).Scan(&examined)
	return
}

// contentPending counts files still awaiting a content pass (new or modified since
// last examined) — the scanner's remaining backlog.
func (s *Store) contentPending() int64 {
	var n int64
	s.db.QueryRow(`
		SELECT COUNT(*)
		FROM files f
		LEFT JOIN content_meta cm ON cm.file_id = f.id
		WHERE f.is_dir = 0 AND (cm.file_id IS NULL OR cm.mtime != f.mtime)`).Scan(&n)
	return n
}
