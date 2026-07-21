package indexer

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// docxBytes builds a minimal .docx (OOXML zip) containing text. extractDocx only
// reads word/document.xml, so that's all we include.
func docxBytes(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, `<?xml version="1.0"?><w:document xmlns:w="ns"><w:body><w:p><w:r><w:t>%s</w:t></w:r></w:p></w:body></w:document>`, text)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pdfBytes builds a minimal single-page PDF containing text, with a byte-accurate
// xref table so pdftotext reads it cleanly.
func pdfBytes(text string) []byte {
	var buf bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	buf.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	content := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", text)
	obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefPos)
	return buf.Bytes()
}

func makeDocx(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.docx")
	if err := os.WriteFile(path, docxBytes(t, text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func makePDF(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(path, pdfBytes(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func TestExtractDocx(t *testing.T) {
	path := makeDocx(t, "the quarterly synergy report is attached")
	text, ok := extractText(path, fileSize(t, path), defaultMaxContentBytes)
	if !ok {
		t.Fatal("docx should extract")
	}
	if !strings.Contains(text, "synergy report") {
		t.Errorf("docx text missing expected content; got %q", text)
	}

	// A non-Word zip (no word/document.xml) is skipped, not errored.
	other := filepath.Join(t.TempDir(), "sheet.docx")
	f, _ := os.Create(other)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("xl/workbook.xml")
	w.Write([]byte("<x/>"))
	zw.Close()
	f.Close()
	if _, ok := extractText(other, fileSize(t, other), defaultMaxContentBytes); ok {
		t.Errorf("a docx without word/document.xml should be skipped")
	}
}

func TestExtractPDF(t *testing.T) {
	if pdftotextPath == "" {
		t.Skip("pdftotext not installed")
	}
	path := makePDF(t, "Hello PDF World invoice 12345")
	text, ok := extractText(path, fileSize(t, path), defaultMaxContentBytes)
	if !ok {
		t.Fatal("pdf should extract")
	}
	if !strings.Contains(text, "invoice 12345") {
		t.Errorf("pdf text missing expected content; got %q", text)
	}
}

// TestExtractDispatch confirms the type switch: a binary file named .txt is still
// rejected by the plain-text path, and unknown-but-textual files extract.
func TestExtractDispatch(t *testing.T) {
	dir := t.TempDir()
	binTxt := filepath.Join(dir, "fake.txt")
	os.WriteFile(binTxt, []byte{0x00, 0x01, 'h', 'i'}, 0o644)
	if _, ok := extractText(binTxt, 4, defaultMaxContentBytes); ok {
		t.Errorf("a .txt with NUL bytes should still be rejected as binary")
	}

	code := filepath.Join(dir, "main.go")
	os.WriteFile(code, []byte("package main\nfunc widget() {}\n"), 0o644)
	text, ok := extractText(code, fileSize(t, code), defaultMaxContentBytes)
	if !ok || !strings.Contains(text, "widget") {
		t.Errorf("source file should extract; ok=%v text=%q", ok, text)
	}
}

// TestContentSearchDocuments runs the full pipeline: the background scanner must
// content-index a .docx (and .pdf, if pdftotext is present) so a word inside the
// document is found by content search.
func TestContentSearchDocuments(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "memo.docx"), docxBytes(t, "the annual budget covers procurement and logistics"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantPDF := pdftotextPath != ""
	if wantPDF {
		if err := os.WriteFile(filepath.Join(root, "scan.pdf"), pdfBytes("shipment manifest reference XR7"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	svc := startContentService(t, root)
	waitFor(t, "documents content-indexed", 5*time.Second, func() bool {
		st, _ := svc.Status()
		want := int64(1)
		if wantPDF {
			want = 2
		}
		return st.ContentIndexed >= want && st.ContentPending == 0
	})

	if n := contentCount(svc, "procurement logistics"); n != 1 {
		t.Errorf("content search inside .docx expected 1, got %d", n)
	}
	if wantPDF {
		if n := contentCount(svc, "shipment manifest"); n != 1 {
			t.Errorf("content search inside .pdf expected 1, got %d", n)
		}
	}
}
