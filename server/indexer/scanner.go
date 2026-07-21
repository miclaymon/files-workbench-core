package indexer

import (
	"context"
	"time"
)

// The content scanner is the low-priority background worker that fills the full-text
// index (Phase 2). It pulls files whose content hasn't been examined at their current
// mtime, extracts their text, and indexes it — throttled so it stays invisible during
// normal use. The name index and live search never wait on it.
//
// It is pull-based off the same `content_meta` bookkeeping the store maintains, so it
// needs no separate queue: a file the watcher just modified reappears in
// FilesNeedingContent (its mtime no longer matches content_meta) and gets re-indexed
// on the next pass. New/changed files are picked up within one idle interval.

type scannerOptions struct {
	batchSize    int           // files examined per iteration
	betweenBatch time.Duration // pause between batches (idle-priority throttle)
	idlePoll     time.Duration // wait when there's nothing to do
	maxBytes     int64         // per-file content cap
	budget       int64         // total indexed-text budget (0 = unlimited)
}

const defaultContentBudget = 512 << 20 // 512 MiB of indexed text

func defaultScannerOptions() scannerOptions {
	return scannerOptions{
		batchSize:    32,
		betweenBatch: 40 * time.Millisecond,
		idlePoll:     2 * time.Second,
		maxBytes:     defaultMaxContentBytes,
		budget:       defaultContentBudget,
	}
}

// runContentScanner processes the content backlog until ctx is cancelled.
func (s *Service) runContentScanner(ctx context.Context, opt scannerOptions) {
	for {
		if ctx.Err() != nil {
			return
		}
		batch, err := s.store.FilesNeedingContent(opt.batchSize)
		if err != nil {
			s.log("content scan query: " + err.Error())
			if !sleep(ctx, opt.idlePoll) {
				return
			}
			continue
		}
		if len(batch) == 0 {
			if !sleep(ctx, opt.idlePoll) { // nothing to do — idle
				return
			}
			continue
		}
		// Track the running indexed-text total across the batch (fetched once, then
		// adjusted as we index) so the budget check doesn't re-SUM per file.
		total := s.store.contentBytes()
		for _, c := range batch {
			if ctx.Err() != nil {
				return
			}
			total += s.indexOneContent(c, opt.maxBytes, opt.budget, total)
		}
		if !sleep(ctx, opt.betweenBatch) { // throttle between batches
			return
		}
	}
}

// indexOneContent examines one file and returns the bytes it ADDED to the content
// index (0 if skipped or a re-index of an already-counted file).
func (s *Service) indexOneContent(c contentCandidate, maxBytes, budget, currentTotal int64) int64 {
	text, metaJSON, ok := extractSearchable(c.Path, c.Size, maxBytes)
	if !ok {
		// Binary / too big / unreadable / no metadata — record so we don't re-examine
		// until it changes.
		if err := s.store.MarkContentSkipped(c.ID, c.MTime, c.Size); err != nil {
			s.log("content mark-skipped " + c.Path + ": " + err.Error())
		}
		return 0
	}
	// Size budget: block indexing a NEW file that would push the content index over
	// budget (a re-index of an already-indexed file is allowed through — it replaces
	// existing content and keeps changed files fresh). An over-budget file is marked
	// examined-but-skipped, so it won't be reconsidered until it changes.
	if budget > 0 && !c.WasIndexed && currentTotal+int64(len(text)) > budget {
		if err := s.store.MarkContentSkipped(c.ID, c.MTime, c.Size); err != nil {
			s.log("content over-budget skip " + c.Path + ": " + err.Error())
		}
		return 0
	}
	if err := s.store.IndexContent(c.ID, text, metaJSON, c.MTime, c.Size); err != nil {
		s.log("content index " + c.Path + ": " + err.Error())
		return 0
	}
	if c.WasIndexed {
		return 0 // replaced existing content — net change is ~0 for the running estimate
	}
	return int64(len(text))
}

// extractSearchable produces the searchable text (and, for media, the structured
// metadata JSON) for a file. Media files are indexed by their embedded metadata;
// everything else by its extracted content text.
func extractSearchable(path string, size, maxBytes int64) (text, metaJSON string, ok bool) {
	if cat := mediaCategory(fileExt(path)); cat != "" {
		meta, ok := extractMediaMetadata(path, cat, size)
		if !ok || len(meta) == 0 {
			return "", "", false
		}
		return metaSearchText(meta), jsonStr(meta), true
	}
	t, ok := extractText(path, size, maxBytes)
	return t, "", ok
}

// sleep waits d or returns false if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
