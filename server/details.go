package main

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"syscall"

	"github.com/dhowden/tag"
)

// handleFsPermissions returns mode string, octal, owner, and group for any path.
func handleFsPermissions(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "Not found: "+path)
		return
	}

	result := map[string]any{
		"mode":  info.Mode().String(),
		"octal": fmt.Sprintf("%04o", info.Mode().Perm()),
	}

	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		result["uid"] = sys.Uid
		result["gid"] = sys.Gid
		if u, err := user.LookupId(fmt.Sprint(sys.Uid)); err == nil {
			result["owner"] = u.Username
		}
		if g, err := user.LookupGroupId(fmt.Sprint(sys.Gid)); err == nil {
			result["group"] = g.Name
		}
	}

	jsonOK(w, result)
}

// handleFsChecksums computes MD5 and SHA-256 for a regular file.
func handleFsChecksums(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if err := checkMediaPath(path); err != nil {
		writeMediaCheckErr(w, err)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "Cannot open file")
		return
	}
	defer f.Close()

	md5h := md5.New()
	sha256h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(md5h, sha256h), f); err != nil {
		jsonErr(w, http.StatusInternalServerError, "Checksum computation failed")
		return
	}

	jsonOK(w, map[string]any{
		"md5":    fmt.Sprintf("%x", md5h.Sum(nil)),
		"sha256": fmt.Sprintf("%x", sha256h.Sum(nil)),
	})
}

// handleMediaExif runs exiftool and returns all metadata as a flat JSON object.
// The client filters by field name to show EXIF, XMP, or IPTC sections.
func handleMediaExif(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	if _, err := os.Stat(path); err != nil {
		jsonErr(w, http.StatusNotFound, "Not found")
		return
	}

	out, err := exec.Command("exiftool", "-json", "-fast2", path).Output()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "exiftool failed: "+err.Error())
		return
	}

	var results []map[string]any
	if err := json.Unmarshal(out, &results); err != nil || len(results) == 0 {
		jsonOK(w, map[string]any{})
		return
	}
	res := results[0]
	for _, k := range []string{"ExifToolVersion", "FileAccessDate", "FileInodeChangeDate"} {
		delete(res, k)
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	jsonOK(w, res)
}

// handleMediaAudioTags reads audio metadata via dhowden/tag (MP3, FLAC, Ogg, M4A, etc.).
func handleMediaAudioTags(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if err := checkMediaPath(path); err != nil {
		writeMediaCheckErr(w, err)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "Cannot open file")
		return
	}
	defer f.Close()

	m, err := tag.ReadFrom(f)
	if err != nil {
		jsonErr(w, http.StatusUnprocessableEntity, "Not a recognized audio format")
		return
	}

	trackNo, trackOf := m.Track()
	discNo, discOf := m.Disc()

	jsonOK(w, map[string]any{
		"title":        m.Title(),
		"artist":       m.Artist(),
		"album_artist": m.AlbumArtist(),
		"album":        m.Album(),
		"year":         m.Year(),
		"genre":        m.Genre(),
		"composer":     m.Composer(),
		"lyrics":       m.Lyrics(),
		"comment":      m.Comment(),
		"track_no":     trackNo,
		"track_of":     trackOf,
		"disc_no":      discNo,
		"disc_of":      discOf,
		"format":       string(m.Format()),
		"file_type":    string(m.FileType()),
	})
}
