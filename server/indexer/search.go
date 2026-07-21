package indexer

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Search runs a normalized Query against the index and returns a page of results.
//
// A substring text query (the default) rides the trigram FTS5 table; prefix/glob
// modes hit the base table directly. Everything else — scope, type/size filters,
// sort, pagination — is composed as WHERE/ORDER clauses so the same shape serves
// both the FTS and non-FTS paths.
func (s *Store) Search(q Query) (ResultPage, error) {
	start := time.Now()
	q = q.withDefaults()

	var (
		joins  []string
		wheres []string
		args   []any
		// relevanceExpr is the bm25() expression to rank/score by, set only when the
		// query rides an FTS table (files_fts for name/path, content_fts for content).
		relevanceExpr string
	)

	if q.Text != "" {
		if q.Content {
			// Full-text content search (Phase 2): AND of the query's tokens against
			// each file's indexed text. Tokens are FTS-quoted so user punctuation can't
			// be parsed as query operators.
			joins = append(joins, "JOIN content_fts ON content_fts.rowid = f.id")
			wheres = append(wheres, "content_fts MATCH ?")
			args = append(args, ftsAndTokens(q.Text))
			relevanceExpr = "bm25(content_fts)"
		} else {
			switch q.Match {
			case MatchPrefix:
				wheres = append(wheres, "f.name LIKE ? ESCAPE '\\'")
				args = append(args, likePrefix(q.Text)+"%")
			case MatchGlob:
				wheres = append(wheres, "f.name GLOB ?")
				args = append(args, q.Text)
			default: // MatchSubstring
				if len([]rune(q.Text)) >= 3 {
					// Trigram FTS needs ≥3 chars. Quote the term as one FTS string so
					// spaces/punctuation are matched literally, not parsed as operators.
					joins = append(joins, "JOIN files_fts ON files_fts.rowid = f.id")
					wheres = append(wheres, "files_fts MATCH ?")
					args = append(args, ftsQuote(q.Text))
					relevanceExpr = "bm25(files_fts)"
				} else {
					// Sub-trigram terms can't use the index — fall back to a scan.
					wheres = append(wheres, "(f.name LIKE ? ESCAPE '\\' OR f.path LIKE ? ESCAPE '\\')")
					lp := "%" + likePrefix(q.Text) + "%"
					args = append(args, lp, lp)
				}
			}
		}
	}

	if q.Scope != "" {
		// Scope restricts to the subtree's *contents* (descendants), not the scope
		// node itself — "search in this folder" semantics.
		wheres = append(wheres, "f.path LIKE ? ESCAPE '\\'")
		args = append(args, likePrefix(normPath(q.Scope))+"/%")
	}
	if len(q.Types) > 0 {
		ph := make([]string, len(q.Types))
		for i, t := range q.Types {
			ph[i] = "?"
			args = append(args, strings.ToLower(strings.TrimPrefix(t, ".")))
		}
		wheres = append(wheres, "f.ext IN ("+strings.Join(ph, ",")+")")
	}
	if q.MinSize > 0 {
		wheres = append(wheres, "f.size >= ?")
		args = append(args, q.MinSize)
	}
	if q.MaxSize > 0 {
		wheres = append(wheres, "f.size <= ?")
		args = append(args, q.MaxSize)
	}
	if q.DirsOnly {
		wheres = append(wheres, "f.is_dir = 1")
	}
	if q.FilesOnly {
		wheres = append(wheres, "f.is_dir = 0")
	}

	from := "FROM files f " + strings.Join(joins, " ")
	where := ""
	if len(wheres) > 0 {
		where = "WHERE " + strings.Join(wheres, " AND ")
	}

	total, err := s.count(from, where, args)
	if err != nil {
		return ResultPage{}, err
	}

	order := orderClause(q, relevanceExpr)
	rowsSQL := fmt.Sprintf(
		"SELECT f.path, f.name, f.ext, f.size, f.mtime, f.is_dir, f.volume_id, f.meta %s %s %s %s LIMIT ? OFFSET ?",
		selectScore(relevanceExpr), from, where, order)
	rows, err := s.db.Query(rowsSQL, append(append([]any{}, args...), q.Limit, q.Offset)...)
	if err != nil {
		return ResultPage{}, err
	}
	defer rows.Close()

	page := ResultPage{Results: []Result{}, Total: total, NextOffset: -1}
	for rows.Next() {
		var (
			r     Result
			mtime int64
			isDir int
			meta  sql.NullString
			score sql.NullFloat64
		)
		if err := rows.Scan(&r.Path, &r.Name, &r.Ext, &r.Size, &mtime, &isDir, &r.VolumeID, &meta, &score); err != nil {
			return ResultPage{}, err
		}
		r.Modified = time.Unix(0, mtime) // stored as unix nanoseconds
		r.IsDir = isDir == 1
		if meta.Valid && meta.String != "" {
			r.Meta = json.RawMessage(meta.String)
		}
		if score.Valid {
			// bm25 is lower-is-better; report a positive "higher is better" score.
			r.Score = -score.Float64
		}
		page.Results = append(page.Results, r)
	}
	if err := rows.Err(); err != nil {
		return ResultPage{}, err
	}
	if q.Offset+len(page.Results) < total {
		page.NextOffset = q.Offset + len(page.Results)
	}
	page.TookMs = time.Since(start).Milliseconds()
	return page, nil
}

func (s *Store) count(from, where string, args []any) (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) "+from+" "+where, args...).Scan(&n)
	return n, err
}

// selectScore adds the bm25 relevance column (from the matched FTS table) only when
// a FTS query supplies it; otherwise a NULL keeps the column count stable for Scan.
func selectScore(relevanceExpr string) string {
	if relevanceExpr != "" {
		return ", " + relevanceExpr
	}
	return ", NULL"
}

func orderClause(q Query, relevanceExpr string) string {
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	switch q.Sort {
	case SortRelevance:
		if relevanceExpr != "" {
			// bm25 ascending = most relevant first; Desc flips to least relevant.
			return "ORDER BY " + relevanceExpr + " " + dir
		}
		return "ORDER BY f.name " + dir // no text query → relevance is meaningless
	case SortSize:
		return "ORDER BY f.size " + dir
	case SortModified:
		return "ORDER BY f.mtime " + dir
	case SortPath:
		return "ORDER BY f.path " + dir
	default:
		return "ORDER BY f.name " + dir + ", f.path " + dir
	}
}

// ftsAndTokens turns a free-text query into an AND of FTS-quoted tokens, so
// "foo bar" matches files containing both words and any punctuation in the input
// is treated literally rather than as FTS query syntax. Empty input → match-all
// guard the caller avoids by only calling this when Text is non-empty.
func ftsAndTokens(text string) string {
	fields := strings.Fields(text)
	for i, f := range fields {
		fields[i] = ftsQuote(f)
	}
	return strings.Join(fields, " ")
}

func (q Query) withDefaults() Query {
	if q.Match == "" {
		q.Match = MatchSubstring
	}
	if q.Sort == "" {
		if q.Text != "" {
			q.Sort = SortRelevance
		} else {
			q.Sort = SortName
		}
	}
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}
	if q.Limit > maxLimit {
		q.Limit = maxLimit
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	return q
}

// ftsQuote wraps a term as a single double-quoted FTS5 string, doubling any internal
// quotes, so the user's raw input can never be parsed as FTS query syntax.
func ftsQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
