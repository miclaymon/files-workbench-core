package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/gif"
	_ "image/png"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/dhowden/tag"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

var allowedThumbSizes = map[int]bool{
	48: true, 64: true, 96: true, 128: true, 256: true, 512: true, 1024: true,
}

// thumbCacheDir returns the platform-appropriate thumbnail cache directory.
func thumbCacheDir() string {
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, "AppData", "Local")
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "Library", "Caches")
	default:
		base = os.Getenv("XDG_CACHE_HOME")
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".cache")
		}
	}
	dir := filepath.Join(base, "files-workbench", "thumbs")
	os.MkdirAll(dir, 0755)
	return dir
}

var _thumbCacheDir = thumbCacheDir()

// thumbCachePath returns the cache file path for a given source file, size, and kind.
// The key format matches the Python implementation so both servers share the same cache.
func thumbCachePath(sourcePath string, size int, kind string) (string, error) {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return "", err
	}
	mtimeNs := info.ModTime().UnixNano()
	key := fmt.Sprintf("%s:%d:%d:%s", sourcePath, mtimeNs, size, kind)
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(_thumbCacheDir, fmt.Sprintf("%x.jpg", hash)), nil
}

// resizeImage generates a JPEG thumbnail for an image file. Returns the JPEG bytes.
// Resizes so height <= size and width <= size*4, maintaining aspect ratio.
// Uses disk cache keyed by path+mtime+size.
func resizeImage(sourcePath string, size int) ([]byte, error) {
	cachePath, err := thumbCachePath(sourcePath, size, "")
	if err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, nil
	}

	f, err := os.Open(sourcePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	src, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", sourcePath, err)
	}

	data, err := encodeThumb(src, size)
	if err != nil {
		return nil, err
	}
	os.WriteFile(cachePath, data, 0644)
	return data, nil
}

// encodeThumb resizes src to fit within (maxW=size*4, maxH=size) and encodes as JPEG.
func encodeThumb(src image.Image, size int) ([]byte, error) {
	origW := src.Bounds().Dx()
	origH := src.Bounds().Dy()
	maxH := size
	maxW := size * 4

	dstW, dstH := computeThumbDims(origW, origH, maxW, maxH)

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func computeThumbDims(origW, origH, maxW, maxH int) (int, int) {
	if origW <= maxW && origH <= maxH {
		return origW, origH
	}
	scaleH := float64(maxH) / float64(origH)
	scaleW := float64(maxW) / float64(origW)
	scale := scaleH
	if scaleW < scaleH {
		scale = scaleW
	}
	w := int(float64(origW) * scale)
	h := int(float64(origH) * scale)
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

// imageConfig reads image dimensions without full decode.
func imageConfig(path string) (image.Config, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, "", err
	}
	defer f.Close()
	return image.DecodeConfig(f)
}

func parseSize(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func clampSize(size int) int {
	if allowedThumbSizes[size] {
		return size
	}
	return 256
}

func isImageMIME(mimeType string) bool {
	return strings.HasPrefix(mimeType, "image/")
}

func isVideoMIME(mimeType string) bool {
	return strings.HasPrefix(mimeType, "video/")
}

func isAudioMIME(mimeType string) bool {
	return strings.HasPrefix(mimeType, "audio/")
}

// audioThumbnail extracts embedded cover art from an audio file, resizes it,
// and caches the result. Returns the JPEG bytes.
func audioThumbnail(path string, size int) ([]byte, error) {
	cachePath, err := thumbCachePath(path, size, "audio")
	if err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	meta, err := tag.ReadFrom(f)
	if err != nil {
		return nil, fmt.Errorf("read tags: %w", err)
	}
	pic := meta.Picture()
	if pic == nil {
		return nil, fmt.Errorf("no embedded artwork in %s", path)
	}

	src, _, err := image.Decode(bytes.NewReader(pic.Data))
	if err != nil {
		return nil, fmt.Errorf("decode artwork: %w", err)
	}

	data, err := encodeThumb(src, size)
	if err != nil {
		return nil, err
	}

	os.WriteFile(cachePath, data, 0644)
	return data, nil
}

// extractAudioArtwork returns the raw embedded cover art bytes (unresized).
func extractAudioArtwork(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	meta, err := tag.ReadFrom(f)
	if err != nil {
		return nil, "", fmt.Errorf("read tags: %w", err)
	}
	pic := meta.Picture()
	if pic == nil {
		return nil, "", fmt.Errorf("no embedded artwork")
	}
	mime := pic.MIMEType
	if mime == "" {
		mime = "image/jpeg"
	}
	return pic.Data, mime, nil
}
