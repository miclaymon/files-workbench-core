package main

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)


func handleMediaCapabilities(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]bool{"thumbnails": true})
}

func handleMediaImage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if err := checkMediaPath(path); err != nil {
		writeMediaCheckErr(w, err)
		return
	}

	sizeStr := q.Get("size")
	if sizeStr != "" {
		size := parseSize(sizeStr, 0)
		if allowedThumbSizes[size] {
			data, err := resizeImage(path, size)
			if err != nil {
				jsonErr(w, http.StatusInternalServerError, "Thumbnail generation failed")
				return
			}
			serveBytes(w, data, "image/jpeg", "public, max-age=3600")
			return
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	serveBytes(w, data, guessMIME(path), "public, max-age=3600")
}

func handleMediaThumbnail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if err := checkMediaPath(path); err != nil {
		writeMediaCheckErr(w, err)
		return
	}

	size := clampSize(parseSize(q.Get("size"), 256))
	mimeType := guessMIME(path)

	if isImageMIME(mimeType) {
		data, err := resizeImage(path, size)
		if err != nil {
			jsonErr(w, http.StatusNotFound, "Thumbnail not available")
			return
		}
		serveBytes(w, data, "image/jpeg", "public, max-age=3600")
		return
	}

	if isVideoMIME(mimeType) {
		data, err := videoThumbnail(path, size)
		if err != nil {
			jsonErr(w, http.StatusNotFound, "Thumbnail not available")
			return
		}
		serveBytes(w, data, "image/jpeg", "public, max-age=3600")
		return
	}

	if isAudioMIME(mimeType) {
		data, err := audioThumbnail(path, size)
		if err != nil {
			jsonErr(w, http.StatusNotFound, "Thumbnail not available")
			return
		}
		serveBytes(w, data, "image/jpeg", "public, max-age=3600")
		return
	}

	jsonErr(w, http.StatusNotFound, "Thumbnail not available")
}

func handleMediaPreview(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if err := checkMediaPath(path); err != nil {
		writeMediaCheckErr(w, err)
		return
	}
	http.ServeFile(w, r, path)
}

func handleMediaPreviewText(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if err := checkMediaPath(path); err != nil {
		writeMediaCheckErr(w, err)
		return
	}

	maxLines := parseSize(q.Get("max_lines"), 100)

	f, err := os.Open(path)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() && len(lines) < maxLines {
		lines = append(lines, scanner.Text())
	}
	if lines == nil {
		lines = []string{}
	}

	jsonOK(w, map[string]any{
		"text":        strings.Join(lines, "\n"),
		"lines":       lines,
		"total_lines": len(lines),
		"encoding":    "utf-8",
	})
}

func handleMediaMetadata(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if err := checkMediaPath(path); err != nil {
		writeMediaCheckErr(w, err)
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "File not found")
		return
	}

	mimeType := guessMIME(path)
	ext := strings.ToLower(filepath.Ext(path))

	meta := map[string]any{
		"name":       info.Name(),
		"path":       path,
		"size_bytes": info.Size(),
		"created":    info.ModTime().Format(time.RFC3339),
		"modified":   info.ModTime().Format(time.RFC3339),
		"extension":  ext,
		"mime_type":  mimeType,
	}

	if isImageMIME(mimeType) {
		if cfg, format, err := imageConfig(path); err == nil {
			meta["format"] = format
			meta["width"] = cfg.Width
			meta["height"] = cfg.Height
			meta["mode"] = "RGB"
			meta["has_transparency"] = false
		}
	}

	jsonOK(w, meta)
}

func handleMediaArtwork(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if err := checkMediaPath(path); err != nil {
		writeMediaCheckErr(w, err)
		return
	}
	data, mime, err := extractAudioArtwork(path)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "No embedded artwork found")
		return
	}
	serveBytes(w, data, mime, "public, max-age=3600")
}

// ── helpers ───────────────────────────────────────────────────────────────────

type mediaCheckError struct {
	status int
	msg    string
}

func (e *mediaCheckError) Error() string { return e.msg }

func checkMediaPath(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return &mediaCheckError{http.StatusNotFound, "File not found"}
	}
	if isExcluded(path, nil) {
		return &mediaCheckError{http.StatusForbidden, "Path is blacklisted"}
	}
	return nil
}

func writeMediaCheckErr(w http.ResponseWriter, err error) {
	if mce, ok := err.(*mediaCheckError); ok {
		jsonErr(w, mce.status, mce.msg)
	} else {
		jsonErr(w, http.StatusInternalServerError, err.Error())
	}
}

func serveBytes(w http.ResponseWriter, data []byte, mimeType, cacheControl string) {
	h := w.Header()
	h.Set("Content-Type", mimeType)
	h.Set("Cache-Control", cacheControl)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// videoThumbnail extracts a thumbnail frame from a video using ffmpeg.
func videoThumbnail(path string, size int) ([]byte, error) {
	cachePath, err := thumbCachePath(path, size, "video")
	if err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, nil
	}

	// Get duration with ffprobe
	var seekSecs float64
	probeOut, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	).Output()
	if err == nil {
		if dur, err := strconv.ParseFloat(strings.TrimSpace(string(probeOut)), 64); err == nil && dur > 0 {
			seekSecs = dur * 0.1
		}
	}

	raw, err := exec.Command("ffmpeg",
		"-ss", fmt.Sprintf("%.3f", seekSecs),
		"-i", path,
		"-vframes", "1",
		"-vf", fmt.Sprintf("scale=-2:%d", size),
		"-f", "image2",
		"-vcodec", "mjpeg",
		"pipe:1",
	).Output()
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("ffmpeg failed for %s: %w", path, err)
	}

	os.WriteFile(cachePath, raw, 0644)
	return raw, nil
}
