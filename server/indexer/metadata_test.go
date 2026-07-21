package indexer

import (
	"encoding/json"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// id3v2 builds a minimal ID3v2.3 tag (title/artist/album) — enough for dhowden/tag
// to recognize the file as MP3 and read its tags.
func id3v2(title, artist, album string) []byte {
	frame := func(id, text string) []byte {
		data := append([]byte{0x00}, []byte(text)...) // 0x00 = ISO-8859-1 encoding byte
		sz := len(data)
		b := append([]byte(id), byte(sz>>24), byte(sz>>16), byte(sz>>8), byte(sz), 0x00, 0x00)
		return append(b, data...)
	}
	var frames []byte
	frames = append(frames, frame("TIT2", title)...)
	frames = append(frames, frame("TPE1", artist)...)
	frames = append(frames, frame("TALB", album)...)
	n := len(frames)
	ss := []byte{byte((n >> 21) & 0x7f), byte((n >> 14) & 0x7f), byte((n >> 7) & 0x7f), byte(n & 0x7f)}
	hdr := append([]byte("ID3"), 0x03, 0x00, 0x00)
	hdr = append(hdr, ss...)
	return append(hdr, frames...)
}

func TestMediaCategoryAndFlatten(t *testing.T) {
	cases := map[string]string{"mp3": "audio", "flac": "audio", "jpg": "image", "heic": "image", "mp4": "video", "mkv": "video", "txt": "", "go": ""}
	for ext, want := range cases {
		if got := mediaCategory(ext); got != want {
			t.Errorf("mediaCategory(%q) = %q, want %q", ext, got, want)
		}
	}
	if fileExt("/a/b/PHOTO.JPG") != "jpg" {
		t.Errorf("fileExt should lowercase and drop the dot")
	}
	// Flatten is deterministic (sorted keys) and includes values.
	text := metaSearchText(map[string]any{"artist": "The Beatles", "album": "Abbey Road", "year": float64(1969)})
	if !strings.Contains(text, "Abbey Road") || !strings.Contains(text, "The Beatles") || !strings.Contains(text, "1969") {
		t.Errorf("flattened text missing values: %q", text)
	}
}

func TestAudioMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "song.mp3")
	if err := os.WriteFile(path, id3v2("Come Together", "The Beatles", "Abbey Road"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, ok := audioMetadata(path)
	if !ok {
		t.Fatal("audio metadata should read")
	}
	if meta["artist"] != "The Beatles" || meta["title"] != "Come Together" || meta["album"] != "Abbey Road" {
		t.Errorf("wrong audio metadata: %+v", meta)
	}
}

func TestImageMetadataExiftool(t *testing.T) {
	if exiftoolPath == "" {
		t.Skip("exiftool not installed")
	}
	path := filepath.Join(t.TempDir(), "photo.jpg")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// Stamp EXIF onto the generated JPEG, then extract it back.
	if err := exec.Command(exiftoolPath, "-overwrite_original", "-Make=TestCam", "-Model=XZ100", path).Run(); err != nil {
		t.Fatalf("exiftool write: %v", err)
	}
	meta, ok := exiftoolMetadata(path, imageTags)
	if !ok {
		t.Fatal("image metadata should read")
	}
	if meta["make"] != "TestCam" || meta["model"] != "XZ100" {
		t.Errorf("wrong image metadata: %+v", meta)
	}
}

func TestMediaMetadataSearch(t *testing.T) {
	root := t.TempDir()
	// An audio file whose ARTIST is not in its filename — found only via metadata.
	if err := os.WriteFile(filepath.Join(root, "track01.mp3"), id3v2("Yesterday", "Paul McCartney", "Help"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := startContentService(t, root)

	waitFor(t, "audio metadata indexed", 5*time.Second, func() bool {
		st, _ := svc.Status()
		return st.FileCount >= 2 && st.ContentIndexed >= 1 && st.ContentPending == 0
	})

	// Content search by the artist (a metadata value, not in the filename) finds it.
	p, err := svc.Search(Query{Text: "McCartney", Content: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Results) != 1 {
		t.Fatalf("content search 'McCartney' expected 1 (via metadata), got %d", len(p.Results))
	}
	// The result carries the structured metadata.
	if len(p.Results[0].Meta) == 0 {
		t.Fatal("result should include media metadata")
	}
	var m map[string]any
	if err := json.Unmarshal(p.Results[0].Meta, &m); err != nil {
		t.Fatalf("meta is not valid JSON: %v", err)
	}
	if m["artist"] != "Paul McCartney" || m["album"] != "Help" {
		t.Errorf("result meta wrong: %+v", m)
	}
	// NAME search for the artist finds nothing (it's only in the metadata).
	if n := countMatches(svc, "McCartney"); n != 0 {
		t.Errorf("name search 'McCartney' should be 0, got %d", n)
	}
}
