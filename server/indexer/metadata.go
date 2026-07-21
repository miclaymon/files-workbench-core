package indexer

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dhowden/tag"
)

// Media metadata extraction (Phase 2). Media files have no extractable *content*
// text, so instead we pull their embedded metadata — audio tags, image EXIF, video
// container info — into a structured JSON blob (stored on files.meta) and a flattened
// searchable string (folded into the content index), so a content search for
// "canon" finds Canon photos and "beatles" finds their tracks.
//
// Audio uses the in-process dhowden/tag; images and video use the external
// `exiftool` (poppler-style optional tool — absent ⇒ those files just aren't indexed).

// maxMediaSourceBytes caps files we'll open/probe for metadata (metadata lives in a
// small header, so a huge media file needn't be read whole — the tools seek).
const maxMediaSourceBytes = 512 << 20 // 512 MiB

var exiftoolPath, _ = exec.LookPath("exiftool")

func mediaCategory(ext string) string {
	switch ext {
	case "mp3", "flac", "m4a", "aac", "ogg", "oga", "opus", "wav", "wma", "aiff", "aif", "ape", "alac":
		return "audio"
	case "jpg", "jpeg", "png", "gif", "tiff", "tif", "heic", "heif", "webp", "bmp",
		"cr2", "cr3", "nef", "arw", "dng", "raf", "orf", "rw2", "pef", "srw":
		return "image"
	case "mp4", "mov", "mkv", "avi", "webm", "m4v", "wmv", "flv", "mpg", "mpeg", "m2ts", "mts", "3gp":
		return "video"
	default:
		return ""
	}
}

// extractMediaMetadata returns curated metadata for a media file, or ok=false when
// there's nothing to index (unreadable, no tags, or the needed tool is absent).
func extractMediaMetadata(path, category string, size int64) (map[string]any, bool) {
	if size > maxMediaSourceBytes {
		return nil, false // never probe oversize media
	}
	switch category {
	case "audio":
		return audioMetadata(path)
	case "image":
		return exiftoolMetadata(path, imageTags)
	case "video":
		return exiftoolMetadata(path, videoTags)
	default:
		return nil, false
	}
}

// audioMetadata reads tags via dhowden/tag (MP3/FLAC/M4A/Ogg/…).
func audioMetadata(path string) (map[string]any, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return nil, false
	}
	out := map[string]any{}
	put := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			out[k] = v
		}
	}
	put("title", m.Title())
	put("artist", m.Artist())
	put("album", m.Album())
	put("albumArtist", m.AlbumArtist())
	put("composer", m.Composer())
	put("genre", m.Genre())
	if y := m.Year(); y != 0 {
		out["year"] = y
	}
	if n, _ := m.Track(); n != 0 {
		out["track"] = n
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// The curated exiftool tag sets — requested explicitly so the output stays small
// and relevant (exiftool otherwise emits hundreds of fields).
var imageTags = []string{
	"Make", "Model", "LensModel", "DateTimeOriginal", "CreateDate",
	"ImageWidth", "ImageHeight", "ISO", "FNumber", "ExposureTime", "FocalLength",
	"GPSLatitude", "GPSLongitude", "Artist", "Copyright", "Title", "Description", "Keywords",
}
var videoTags = []string{
	"Duration", "ImageWidth", "ImageHeight", "VideoFrameRate", "CompressorName",
	"Title", "Artist", "Author", "CreateDate", "Make", "Model", "GPSCoordinates",
}

// exiftoolMetadata runs exiftool for the wanted tags and returns them as a map.
// Optional: with exiftool absent it returns ok=false and the file isn't indexed.
func exiftoolMetadata(path string, wanted []string) (map[string]any, bool) {
	if exiftoolPath == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"-json", "-fast2", "-n"} // -n: numeric GPS/values, not formatted
	for _, tag := range wanted {
		args = append(args, "-"+tag)
	}
	args = append(args, path)
	out, err := exec.CommandContext(ctx, exiftoolPath, args...).Output()
	if err != nil {
		return nil, false
	}
	// exiftool -json emits a one-element array of objects.
	var arr []map[string]any
	if json.Unmarshal(out, &arr) != nil || len(arr) == 0 {
		return nil, false
	}
	meta := map[string]any{}
	for k, v := range arr[0] {
		if k == "SourceFile" {
			continue
		}
		meta[lowerFirst(k)] = v
	}
	if len(meta) == 0 {
		return nil, false
	}
	return meta, true
}

// metaSearchText flattens a metadata map's values into a stable, searchable string
// (deterministic order for reproducible content hashes). Numbers and strings are
// included; nested/other types are JSON-encoded compactly.
func metaSearchText(meta map[string]any) string {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		switch v := meta[k].(type) {
		case string:
			b.WriteString(v)
		case float64, int, int64, bool:
			b.WriteString(strings.TrimSpace(strings.Trim(jsonStr(v), `"`)))
		default:
			b.WriteString(jsonStr(v))
		}
		b.WriteByte(' ')
	}
	return strings.TrimSpace(b.String())
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// fileExt returns a path's lowercased extension without the leading dot ("" for none).
func fileExt(path string) string {
	e := filepath.Ext(path)
	if e == "" {
		return ""
	}
	return strings.ToLower(e[1:])
}
