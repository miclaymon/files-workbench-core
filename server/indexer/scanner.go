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
}

func defaultScannerOptions() scannerOptions {
	return scannerOptions{
		batchSize:    32,
		betweenBatch: 40 * time.Millisecond,
		idlePoll:     2 * time.Second,
		maxBytes:     defaultMaxContentBytes,
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
		for _, c := range batch {
			if ctx.Err() != nil {
				return
			}
			s.indexOneContent(c, opt.maxBytes)
		}
		if !sleep(ctx, opt.betweenBatch) { // throttle between batches
			return
		}
	}
}

func (s *Service) indexOneContent(c contentCandidate, maxBytes int64) {
	text, ok := extractText(c.Path, c.Size, maxBytes)
	if !ok {
		// Binary / too big / unreadable — record so we don't re-examine until it changes.
		if err := s.store.MarkContentSkipped(c.ID, c.MTime, c.Size); err != nil {
			s.log("content mark-skipped " + c.Path + ": " + err.Error())
		}
		return
	}
	if err := s.store.IndexContent(c.ID, text, c.MTime, c.Size); err != nil {
		s.log("content index " + c.Path + ": " + err.Error())
	}
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
