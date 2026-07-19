package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"unicode/utf16"
)

// Resource type IDs (RT_*)
const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16
)

// ── version info ──────────────────────────────────────────────────────────────

type exeInfo struct {
	Name        string `json:"name,omitempty"`
	Publisher   string `json:"publisher,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

func handleMediaExeInfo(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "path required")
		return
	}
	if isExcluded(path, nil) {
		jsonErr(w, http.StatusForbidden, "Path is blacklisted")
		return
	}
	info, err := readExeInfo(path)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if info == nil {
		jsonOK(w, map[string]any{})
		return
	}
	jsonOK(w, info)
}

func readExeInfo(path string) (*exeInfo, error) {
	data, err := peResource(path, rtVersion, 1, true)
	if err != nil || data == nil {
		return nil, nil // no version resource — not an error
	}
	strs := parseVersionStrings(data)
	if len(strs) == 0 {
		return nil, nil
	}
	info := &exeInfo{
		Name:        firstNonEmpty(strs["ProductName"], strs["FileDescription"]),
		Publisher:   strs["CompanyName"],
		Version:     firstNonEmpty(strs["FileVersion"], strs["ProductVersion"]),
		Description: strs["FileDescription"],
	}
	return info, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ── icon endpoint ─────────────────────────────────────────────────────────────

func handleMediaExeIcon(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.NotFound(w, r)
		return
	}
	if isExcluded(path, nil) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	icon, ct, err := extractExeIcon(path)
	if err != nil || icon == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(icon)
}

func extractExeIcon(path string) ([]byte, string, error) {
	groupData, err := peResource(path, rtGroupIcon, 0, false) // first available ID
	if err != nil || len(groupData) < 6 {
		return nil, "", err
	}

	count := int(binary.LittleEndian.Uint16(groupData[4:6]))
	if count == 0 {
		return nil, "", nil
	}

	// GRPICONDIRENTRY: width(1)+height(1)+colors(1)+reserved(1)+planes(2)+bpp(2)+size(4)+id(2) = 14 bytes
	type grpEntry struct{ w, h uint8; bpp uint16; id uint16; size uint32 }
	best := grpEntry{}
	for i := 0; i < count; i++ {
		off := 6 + i*14
		if off+14 > len(groupData) {
			break
		}
		e := grpEntry{
			w:    groupData[off],
			h:    groupData[off+1],
			bpp:  binary.LittleEndian.Uint16(groupData[off+6:]),
			id:   binary.LittleEndian.Uint16(groupData[off+12:]),
			size: binary.LittleEndian.Uint32(groupData[off+8:]),
		}
		if iconScore(e.w, e.bpp) > iconScore(best.w, best.bpp) {
			best = e
		}
	}
	if best.id == 0 {
		return nil, "", nil
	}

	iconData, err := peResource(path, rtIcon, uint32(best.id), true)
	if err != nil || iconData == nil {
		return nil, "", err
	}

	// Modern PE files (Vista+) store icons as PNG directly in resources.
	if bytes.HasPrefix(iconData, []byte("\x89PNG")) {
		return iconData, "image/png", nil
	}

	// Legacy DIB — wrap it in a minimal .ico container so browsers can render it.
	ico := wrapInIco(iconData, best.w, best.h, best.bpp, best.size)
	return ico, "image/x-icon", nil
}

func iconScore(w uint8, bpp uint16) int {
	s := int(w)
	if s == 0 {
		s = 256 // 0 means 256px in ICO format
	}
	return s*100 + int(bpp)
}

// wrapInIco wraps raw DIB resource bytes in a minimal single-image ICO file.
func wrapInIco(dib []byte, w, h uint8, bpp uint16, size uint32) []byte {
	const dataOffset = 6 + 16 // ICONDIR(6) + one ICONDIRENTRY(16)
	buf := make([]byte, 0, dataOffset+int(size))
	// ICONDIR: reserved(2) + type=1(2) + count=1(2)
	buf = append(buf, 0, 0, 1, 0, 1, 0)
	// ICONDIRENTRY
	buf = append(buf,
		w, h, 0, 0, // bWidth, bHeight, bColorCount, reserved
		1, 0,       // wPlanes
		byte(bpp), byte(bpp>>8),   // wBitCount
		byte(size), byte(size>>8), byte(size>>16), byte(size>>24), // dwBytesInRes
		byte(dataOffset), byte(dataOffset>>8), byte(dataOffset>>16), byte(dataOffset>>24),
	)
	buf = append(buf, dib...)
	return buf
}

// ── PE resource reader ────────────────────────────────────────────────────────

// peResource reads raw bytes of a single resource from a Windows PE file.
// resType: RT_* constant. resID: numeric ID (0 = first available, exactID=false).
// Returns nil, nil when the resource simply doesn't exist.
func peResource(path string, resType, resID uint32, exactID bool) ([]byte, error) {
	f, err := pe.Open(path)
	if err != nil {
		return nil, nil // not a PE file — not an error
	}
	defer f.Close()

	rsrc := f.Section(".rsrc")
	if rsrc == nil {
		return nil, nil // no resource section
	}

	secData, err := io.ReadAll(rsrc.Open())
	if err != nil {
		return nil, err
	}

	rr := rsrcReader{data: secData, secRVA: rsrc.VirtualAddress}
	return rr.find(resType, resID, exactID)
}

// rsrcReader navigates an IMAGE_RESOURCE_DIRECTORY tree from raw section bytes.
type rsrcReader struct {
	data   []byte
	secRVA uint32
}

// find walks type → id → language levels and returns the raw resource bytes.
func (r *rsrcReader) find(resType, resID uint32, exactID bool) ([]byte, error) {
	// Level 1: resource type
	typeDir, ok := r.childDir(0, resType, true)
	if !ok {
		return nil, nil
	}
	// Level 2: resource ID
	var idDir uint32
	if exactID || resID != 0 {
		idDir, ok = r.childDir(typeDir, resID, exactID)
	} else {
		idDir, ok = r.firstChild(typeDir)
	}
	if !ok {
		return nil, nil
	}
	// Level 3: language — take the first one
	langRef, ok := r.firstChild(idDir)
	if !ok {
		return nil, nil
	}
	return r.readData(langRef)
}

// childDir scans a directory for an entry with the given numeric ID.
// Returns (directory offset, found). If not exactID, returns the first ID entry.
func (r *rsrcReader) childDir(dirOff, id uint32, exactID bool) (uint32, bool) {
	d := int(dirOff)
	if d+16 > len(r.data) {
		return 0, false
	}
	named := int(binary.LittleEndian.Uint16(r.data[d+12:]))
	ids := int(binary.LittleEndian.Uint16(r.data[d+14:]))

	for i := 0; i < named+ids; i++ {
		e := d + 16 + i*8
		if e+8 > len(r.data) {
			break
		}
		nameField := binary.LittleEndian.Uint32(r.data[e:])
		dataField := binary.LittleEndian.Uint32(r.data[e+4:])
		isNamed := nameField&0x80000000 != 0
		entryID := nameField & 0x7FFFFFFF

		if exactID {
			if isNamed || entryID != id {
				continue
			}
		} else if isNamed {
			continue // skip named entries when looking for first numeric ID
		}

		// Entries at type/id levels always point to subdirectories (MSB=1)
		if dataField&0x80000000 != 0 {
			return dataField & 0x7FFFFFFF, true
		}
		return dataField, true
	}
	return 0, false
}

// firstChild returns the offset of the first child in a directory.
func (r *rsrcReader) firstChild(dirOff uint32) (uint32, bool) {
	d := int(dirOff)
	if d+16 > len(r.data) {
		return 0, false
	}
	named := int(binary.LittleEndian.Uint16(r.data[d+12:]))
	ids := int(binary.LittleEndian.Uint16(r.data[d+14:]))
	if named+ids == 0 {
		return 0, false
	}
	e := d + 16
	if e+8 > len(r.data) {
		return 0, false
	}
	dataField := binary.LittleEndian.Uint32(r.data[e+4:])
	return dataField & 0x7FFFFFFF, true
}

// readData reads raw bytes from an IMAGE_RESOURCE_DATA_ENTRY.
func (r *rsrcReader) readData(entryOff uint32) ([]byte, error) {
	e := int(entryOff)
	if e+16 > len(r.data) {
		return nil, nil
	}
	// IMAGE_RESOURCE_DATA_ENTRY: OffsetToData(4) + Size(4) + CodePage(4) + Reserved(4)
	rva := binary.LittleEndian.Uint32(r.data[e:])
	size := binary.LittleEndian.Uint32(r.data[e+4:])
	if rva < r.secRVA {
		return nil, nil // RVA outside our section
	}
	off := rva - r.secRVA
	if int(off)+int(size) > len(r.data) {
		return nil, nil
	}
	out := make([]byte, size)
	copy(out, r.data[off:])
	return out, nil
}

// ── VS_VERSIONINFO parser ─────────────────────────────────────────────────────

// parseVersionStrings walks a VS_VERSIONINFO block and returns the StringFileInfo key→value pairs.
// All string values are UTF-16LE encoded in the binary.
func parseVersionStrings(data []byte) map[string]string {
	result := make(map[string]string)
	if len(data) < 40 {
		return result
	}

	// Top-level VS_VERSIONINFO block:
	//   wLength(2) + wValueLength(2) + wType(2) + szKey("VS_VERSION_INFO\0" in UTF-16LE, 32 bytes)
	//   + padding + VS_FIXEDFILEINFO (wValueLength bytes) + padding + children
	totalLen := int(binary.LittleEndian.Uint16(data[0:2]))
	if totalLen > len(data) {
		totalLen = len(data)
	}
	valueLen := int(binary.LittleEndian.Uint16(data[2:4]))

	// Skip past the root block's header and value to reach its children.
	// Key "VS_VERSION_INFO\0" = 16 UTF-16 chars = 32 bytes.
	afterKey := vAlign(6 + 32)
	childrenStart := vAlign(afterKey + valueLen)
	if childrenStart >= totalLen {
		return result
	}

	walkChildren(data, childrenStart, totalLen, result)
	return result
}

func walkChildren(data []byte, pos, limit int, out map[string]string) {
	for pos+6 < limit {
		blockLen := int(binary.LittleEndian.Uint16(data[pos:]))
		if blockLen == 0 {
			break
		}
		blockEnd := pos + blockLen
		if blockEnd > limit {
			blockEnd = limit
		}
		valueLen := int(binary.LittleEndian.Uint16(data[pos+2:]))

		key, keyBytes := readU16Z(data[pos+6:])
		dataOff := vAlign(pos + 6 + keyBytes)

		switch key {
		case "StringFileInfo":
			// StringFileInfo has no value; walk StringTable children directly.
			walkStringTables(data, dataOff, blockEnd, out)
		}

		// Advance past this block (aligned to 4 bytes).
		_ = valueLen
		pos = vAlign(blockEnd)
	}
}

func walkStringTables(data []byte, pos, limit int, out map[string]string) {
	for pos+6 < limit {
		tableLen := int(binary.LittleEndian.Uint16(data[pos:]))
		if tableLen == 0 {
			break
		}
		tableEnd := pos + tableLen
		if tableEnd > limit {
			tableEnd = limit
		}

		// StringTable key is a language/codepage like "040904B0"; skip past it.
		_, tkBytes := readU16Z(data[pos+6:])
		sPos := vAlign(pos + 6 + tkBytes) // StringTable has no Value

		for sPos+6 < tableEnd {
			sLen := int(binary.LittleEndian.Uint16(data[sPos:]))
			if sLen == 0 {
				break
			}
			sValLen := int(binary.LittleEndian.Uint16(data[sPos+2:]))
			sKey, skBytes := readU16Z(data[sPos+6:])
			sValOff := vAlign(sPos + 6 + skBytes)

			// sValLen for String blocks is in UTF-16 WORDs (characters).
			if sValLen > 0 && sValOff < tableEnd {
				end := sValOff + sValLen*2
				if end > tableEnd {
					end = tableEnd
				}
				val, _ := readU16Z(data[sValOff:end])
				out[sKey] = strings.TrimRight(val, "\x00 ")
			}

			sPos = vAlign(sPos + sLen)
		}

		pos = vAlign(tableEnd)
	}
}

// vAlign aligns n to the nearest 4-byte (DWORD) boundary.
func vAlign(n int) int { return (n + 3) &^ 3 }

// readU16Z decodes a NUL-terminated UTF-16LE string.
// Returns (string, bytes consumed including the NUL terminator).
func readU16Z(data []byte) (string, int) {
	var u []uint16
	for i := 0; i+1 < len(data); i += 2 {
		ch := binary.LittleEndian.Uint16(data[i:])
		if ch == 0 {
			return string(utf16.Decode(u)), i + 2
		}
		u = append(u, ch)
	}
	return string(utf16.Decode(u)), len(data)
}
