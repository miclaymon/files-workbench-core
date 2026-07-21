//go:build windows

package indexer

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Low-level NTFS USN change-journal access (Phase 3, Windows).
//
// ⚠️ RUNTIME-UNTESTED. Written against the Win32 documentation and verified only to
// cross-compile from Linux — it has not been run on Windows. The higher layer
// (source_windows.go) falls back to the portable backend on any error here, so a
// setup failure (no elevation, non-NTFS volume) degrades gracefully; logic bugs that
// don't error would need a Windows host to surface. Treat as a starting point to
// validate on Windows, not as verified code.
//
// The USN journal records every change to an NTFS volume as a monotonically
// increasing USN. We query the journal for its ID + current USN, then read forward
// from a persisted cursor — giving whole-volume live monitoring with cross-restart
// catch-up (no per-directory watches, no re-walk).

const (
	fsctlQueryUsnJournal = 0x000900f4
	fsctlReadUsnJournal  = 0x000900bb

	usnReasonFileCreate    = 0x00000100
	usnReasonFileDelete    = 0x00000200
	usnReasonRenameNewName = 0x00002000
	usnReasonRenameOldName = 0x00001000
	usnReasonDataOverwrite = 0x00000001
	usnReasonDataExtend    = 0x00000002
	usnReasonDataTrunc     = 0x00000004
	usnReasonClose         = 0x80000000

	fileAttributeDirectory = 0x00000010

	fileNameNormalized = 0x0 // GetFinalPathNameByHandle: normalized path, DOS volume name
)

// usnJournalData mirrors USN_JOURNAL_DATA_V0 (56 bytes).
type usnJournalData struct {
	UsnJournalID    uint64
	FirstUsn        int64
	NextUsn         int64
	LowestValidUsn  int64
	MaxUsn          int64
	MaximumSize     uint64
	AllocationDelta uint64
}

// usnRecord is a parsed USN_RECORD_V2 (the fields we use).
type usnRecord struct {
	FileRefNumber       uint64
	ParentFileRefNumber uint64
	Usn                 int64
	TimeStamp           time.Time
	Reason              uint32
	Attributes          uint32
	FileName            string
}

func (r usnRecord) isDir() bool { return r.Attributes&fileAttributeDirectory != 0 }

// openVolume opens the raw NTFS volume backing a drive letter (e.g. "C"), for the
// FSCTL journal calls. Requires an elevated handle in practice.
func openVolume(letter string) (windows.Handle, error) {
	path := `\\.\` + strings.ToUpper(letter) + ":"
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("open volume %s: %w", path, err)
	}
	return h, nil
}

// queryJournal reads the volume's active USN journal metadata.
func queryJournal(h windows.Handle) (usnJournalData, error) {
	var out usnJournalData
	var ret uint32
	err := windows.DeviceIoControl(h, fsctlQueryUsnJournal,
		nil, 0,
		(*byte)(unsafe.Pointer(&out)), uint32(unsafe.Sizeof(out)),
		&ret, nil)
	if err != nil {
		return usnJournalData{}, fmt.Errorf("query usn journal: %w", err)
	}
	return out, nil
}

// readUsnJournalDataV0 mirrors READ_USN_JOURNAL_DATA_V0 (40 bytes).
type readUsnJournalDataV0 struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	UsnJournalID      uint64
}

// readJournal reads one batch of USN records starting at startUsn. Returns the parsed
// records and the next USN to resume from. A returned nextUsn == startUsn with no
// records means the journal is caught up.
func readJournal(h windows.Handle, journalID uint64, startUsn int64) (recs []usnRecord, nextUsn int64, err error) {
	in := readUsnJournalDataV0{
		StartUsn:     startUsn,
		ReasonMask:   0xFFFFFFFF,
		UsnJournalID: journalID,
	}
	buf := make([]byte, 64*1024) // the first 8 bytes are the next USN; records follow
	var ret uint32
	err = windows.DeviceIoControl(h, fsctlReadUsnJournal,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		&buf[0], uint32(len(buf)),
		&ret, nil)
	if err != nil {
		return nil, startUsn, fmt.Errorf("read usn journal: %w", err)
	}
	if ret < 8 {
		return nil, startUsn, nil
	}
	nextUsn = int64(binary.LittleEndian.Uint64(buf[:8]))
	recs = parseUsnRecords(buf[8:ret])
	return recs, nextUsn, nil
}

// parseUsnRecords walks a USN output buffer of variable-length USN_RECORD_V2 entries.
func parseUsnRecords(b []byte) []usnRecord {
	var out []usnRecord
	for len(b) >= 60 {
		recLen := binary.LittleEndian.Uint32(b[0:4])
		if recLen < 60 || int(recLen) > len(b) {
			break
		}
		major := binary.LittleEndian.Uint16(b[4:6])
		if major != 2 { // only V2 is parsed here; V3 uses 128-bit refs
			b = b[recLen:]
			continue
		}
		nameLen := binary.LittleEndian.Uint16(b[56:58])
		nameOff := binary.LittleEndian.Uint16(b[58:60])
		var name string
		if int(nameOff)+int(nameLen) <= int(recLen) {
			name = decodeUTF16(b[nameOff : int(nameOff)+int(nameLen)])
		}
		out = append(out, usnRecord{
			FileRefNumber:       binary.LittleEndian.Uint64(b[8:16]),
			ParentFileRefNumber: binary.LittleEndian.Uint64(b[16:24]),
			Usn:                 int64(binary.LittleEndian.Uint64(b[24:32])),
			TimeStamp:           filetimeToTime(int64(binary.LittleEndian.Uint64(b[32:40]))),
			Reason:              binary.LittleEndian.Uint32(b[40:44]),
			Attributes:          binary.LittleEndian.Uint32(b[52:56]),
			FileName:            name,
		})
		b = b[recLen:]
	}
	return out
}

func decodeUTF16(b []byte) string {
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return windows.UTF16ToString(u16)
}

// filetimeToTime converts a Windows FILETIME (100ns ticks since 1601) to time.Time.
func filetimeToTime(ft int64) time.Time {
	if ft == 0 {
		return time.Time{}
	}
	const ticksPerSecond = 10_000_000
	const epochDiffSeconds = 11644473600 // 1601→1970
	sec := ft/ticksPerSecond - epochDiffSeconds
	nsec := (ft % ticksPerSecond) * 100
	return time.Unix(sec, nsec)
}

// resolveByID resolves a file-reference number to a full path via OpenFileById +
// GetFinalPathNameByHandle. Used to turn a USN record's parent ref into a path.
// OpenFileById isn't wrapped by x/sys/windows, so it's called through kernel32.
var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procOpenFileByID = kernel32.NewProc("OpenFileById")
)

// fileIDDescriptor mirrors FILE_ID_DESCRIPTOR with a 64-bit FileId (Type = 0).
// Layout: dwSize(4) + Type(4) + union(16, GUID-sized) — we use the first 8 as FileId.
type fileIDDescriptor struct {
	dwSize uint32
	idType uint32
	fileID uint64
	_      uint64 // union padding to GUID size
}

func resolveByID(vol windows.Handle, ref uint64) (string, error) {
	desc := fileIDDescriptor{dwSize: 24, idType: 0, fileID: ref}
	r, _, err := procOpenFileByID.Call(
		uintptr(vol),
		uintptr(unsafe.Pointer(&desc)),
		uintptr(windows.GENERIC_READ),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE),
		0,
		uintptr(windows.FILE_FLAG_BACKUP_SEMANTICS), // required to open directories
	)
	h := windows.Handle(r)
	if h == windows.InvalidHandle {
		return "", fmt.Errorf("OpenFileById(%d): %w", ref, err)
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized)
	if err != nil {
		return "", err
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		if _, err = windows.GetFinalPathNameByHandle(h, &buf[0], n, fileNameNormalized); err != nil {
			return "", err
		}
	}
	// Strip the \\?\ extended-length prefix Windows returns.
	return strings.TrimPrefix(windows.UTF16ToString(buf), `\\?\`), nil
}
