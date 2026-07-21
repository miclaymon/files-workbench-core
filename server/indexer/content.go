package indexer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Content extraction for the full-text index (Phase 2). Dispatches by file type:
// plain UTF-8 text/code directly, .docx via the stdlib zip/XML, and .pdf via the
// external `pdftotext` (optional — degrades gracefully when absent). More document
// types plug into extractText's switch.

// Default limits — a host can tune these via the scanner options.
const (
	defaultMaxContentBytes = 2 << 20  // 2 MiB — extracted-text cap per file
	maxDocSourceBytes      = 16 << 20 // documents may be larger than the text cap (they compress)
	sniffBytes             = 8192     // bytes read to decide text-vs-binary
)

// pdftotextPath is the resolved `pdftotext` binary (poppler-utils), or "" when the
// tool isn't installed — in which case PDFs are simply not content-indexed.
var pdftotextPath, _ = exec.LookPath("pdftotext")

// extractText reads a file's indexable text, or returns ok=false when it should be
// skipped (too large, binary, unreadable, or an unsupported type).
func extractText(path string, size, maxBytes int64) (text string, ok bool) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxContentBytes
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".docx":
		return extractDocx(path, size, maxBytes)
	case ".pdf":
		return extractPDF(path, size, maxBytes)
	default:
		return extractPlainText(path, size, maxBytes)
	}
}

// extractPlainText reads a UTF-8 text/code file (the common case), rejecting binary
// content. It reads at most maxBytes so a giant or pathological file can't blow up
// memory.
func extractPlainText(path string, size, maxBytes int64) (text string, ok bool) {
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

// extractDocx pulls the text from a .docx (an OOXML zip): the body lives in
// word/document.xml, where the visible text is the XML character data between the
// run tags. Collecting all CharData yields the document text without needing to
// model the full schema.
func extractDocx(path string, size, maxBytes int64) (string, bool) {
	if size > maxDocSourceBytes {
		return "", false
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", false
	}
	defer zr.Close()

	var body *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			body = f
			break
		}
	}
	if body == nil {
		return "", false // not a Word document (could be xlsx/pptx — handled later)
	}
	rc, err := body.Open()
	if err != nil {
		return "", false
	}
	defer rc.Close()

	var b strings.Builder
	dec := xml.NewDecoder(io.LimitReader(rc, maxDocSourceBytes))
	for {
		tok, err := dec.Token()
		if err != nil {
			break // EOF or malformed tail — keep what we have
		}
		if cd, ok := tok.(xml.CharData); ok {
			b.Write(cd)
			b.WriteByte(' ') // separate runs so adjacent words don't merge
			if int64(b.Len()) >= maxBytes {
				break
			}
		}
	}
	return finishExtract(b.String(), maxBytes)
}

// extractPDF shells out to `pdftotext` (poppler-utils). Optional: when the tool
// isn't installed, PDFs are just not content-indexed. A timeout guards against a
// pathological file hanging the scanner.
func extractPDF(path string, size, maxBytes int64) (string, bool) {
	if pdftotextPath == "" || size > maxDocSourceBytes {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// "-" writes extracted text to stdout; -q silences warnings.
	out, err := exec.CommandContext(ctx, pdftotextPath, "-q", "-enc", "UTF-8", path, "-").Output()
	if err != nil {
		return "", false
	}
	if int64(len(out)) > maxBytes {
		out = out[:maxBytes]
	}
	return finishExtract(string(out), maxBytes)
}

// finishExtract validates and caps extracted text: reject empty/invalid-UTF-8,
// truncate to the content cap.
func finishExtract(text string, maxBytes int64) (string, bool) {
	if int64(len(text)) > maxBytes {
		text = text[:maxBytes]
	}
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}
