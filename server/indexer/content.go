package indexer

import (
	"bytes"
	"io"
	"os"
	"unicode/utf8"
)

// Content extraction for the full-text index (Phase 2). v1 handles UTF-8 text and
// source files; richer per-type extractors (PDF, docx, …) plug in here later.

// Default limits — a host can tune these via the scanner options.
const (
	defaultMaxContentBytes = 2 << 20 // 2 MiB — skip larger files (cheap name-search still covers them)
	sniffBytes             = 8192    // bytes read to decide text-vs-binary
)

// extractText reads a file's indexable text, or returns ok=false when it should be
// skipped (too large, binary, or unreadable). It reads at most maxBytes so a giant
// or pathological file can't blow up memory.
func extractText(path string, size, maxBytes int64) (text string, ok bool) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxContentBytes
	}
	if size > maxBytes {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	// Sniff the head for binary content (a NUL byte, or mostly-invalid UTF-8).
	head := make([]byte, sniffBytes)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if !looksTextual(head) {
		return "", false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", false
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return "", false
	}
	// Final guard: ensure the whole read is valid UTF-8 (drop otherwise — the
	// tokenizer expects text, and this keeps binary that sniffed clean out).
	if !utf8.Valid(buf) {
		return "", false
	}
	return string(buf), true
}

// looksTextual reports whether a byte sample is plausibly text: no NUL bytes and a
// low proportion of non-printable/control bytes.
func looksTextual(b []byte) bool {
	if len(b) == 0 {
		return true // empty file — trivially "text"
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return false // NUL ⇒ binary
	}
	var suspicious int
	for _, c := range b {
		// Allow tab/newline/carriage-return; count other control chars as suspicious.
		if c < 0x09 || (c > 0x0d && c < 0x20) {
			suspicious++
		}
	}
	return suspicious*100/len(b) < 10 // <10% control bytes
}
