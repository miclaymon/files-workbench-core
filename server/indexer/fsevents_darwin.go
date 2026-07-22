//go:build darwin

package indexer

/*
#cgo LDFLAGS: -framework CoreServices -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdlib.h>

// goFSEventsCallback is implemented in Go (see the //export below) and handed to
// FSEventStreamCreate as the FSEventStreamCallback. It's declared here with the
// exact signature FSEventStreamCallback expects (FSEvents.h) so the cast in
// fw_create_stream is well-typed rather than a blind function-pointer cast.
extern void goFSEventsCallback(
    ConstFSEventStreamRef streamRef,
    void *clientCallBackInfo,
    size_t numEvents,
    char **eventPaths,
    FSEventStreamEventFlags *eventFlags,
    FSEventStreamEventId *eventIds);

// fw_create_stream builds the CFArray FSEventStreamCreate requires from a plain
// C string array, wires up the context (info carries a runtime/cgo.Handle, passed
// as a uintptr_t so no unsafe.Pointer<->uintptr conversion is needed on the Go
// side — see newFSEventStream), and creates the stream. Always requests
// per-item (kFSEventStreamCreateFlagFileEvents) events without CFTypes, so the
// callback receives plain eventPaths as char** — no CFString marshaling needed
// per event, only for the one-time input path array built here.
static FSEventStreamRef fw_create_stream(char **paths, int pathCount, uintptr_t info,
                                          FSEventStreamEventId since, CFTimeInterval latency) {
    CFMutableArrayRef arr = CFArrayCreateMutable(NULL, pathCount, &kCFTypeArrayCallBacks);
    for (int i = 0; i < pathCount; i++) {
        CFStringRef s = CFStringCreateWithCString(NULL, paths[i], kCFStringEncodingUTF8);
        if (s != NULL) {
            CFArrayAppendValue(arr, s);
            CFRelease(s);
        }
    }

    FSEventStreamContext ctx;
    ctx.version = 0;
    ctx.info = (void *)info;
    ctx.retain = NULL;
    ctx.release = NULL;
    ctx.copyDescription = NULL;

    FSEventStreamCreateFlags flags = kFSEventStreamCreateFlagFileEvents |
                                      kFSEventStreamCreateFlagNoDefer |
                                      kFSEventStreamCreateFlagWatchRoot;

    FSEventStreamRef stream = FSEventStreamCreate(
        NULL, (FSEventStreamCallback)goFSEventsCallback, &ctx, arr, since, latency, flags);
    CFRelease(arr);
    return stream;
}

// fw_start schedules the stream on a private serial dispatch queue and starts it.
// A dispatch queue (rather than a CFRunLoop) avoids needing to dedicate + pin an OS
// thread and pump CFRunLoopRun() ourselves — Apple's recommended approach since the
// dispatch-queue scheduling API was added (10.9).
static dispatch_queue_t fw_start(FSEventStreamRef stream) {
    dispatch_queue_t q = dispatch_queue_create("com.filesworkbench.fsevents", NULL);
    FSEventStreamSetDispatchQueue(stream, q);
    FSEventStreamStart(stream);
    return q;
}

// fw_stop_and_release tears a stream down in Apple's documented order: Stop, then
// Invalidate (detaches it from the queue), then Release, then release our queue.
static void fw_stop_and_release(FSEventStreamRef stream, dispatch_queue_t q) {
    FSEventStreamStop(stream);
    FSEventStreamInvalidate(stream);
    FSEventStreamRelease(stream);
    dispatch_release(q);
}

// fw_fsevents_uuid_for_device wraps FSEventsCopyUUIDForDevice, returning a
// malloc'd C string the caller must free(), or NULL if the device isn't
// FSEvents-eligible (e.g. a network volume) or has no journal.
static char *fw_fsevents_uuid_for_device(int32_t dev) {
    CFUUIDRef uuid = FSEventsCopyUUIDForDevice((dev_t)dev);
    if (uuid == NULL) {
        return NULL;
    }
    CFStringRef s = CFUUIDCreateString(NULL, uuid);
    CFRelease(uuid);
    if (s == NULL) {
        return NULL;
    }
    CFIndex len = CFStringGetLength(s);
    CFIndex maxSize = CFStringGetMaximumSizeForEncoding(len, kCFStringEncodingUTF8) + 1;
    char *buf = (char *)malloc((size_t)maxSize);
    if (buf == NULL || !CFStringGetCString(s, buf, maxSize, kCFStringEncodingUTF8)) {
        free(buf);
        CFRelease(s);
        return NULL;
    }
    CFRelease(s);
    return buf;
}
*/
import "C"

import (
	"errors"
	"runtime/cgo"
	"time"
	"unsafe"
)

// Low-level FSEvents access (Phase 3, macOS). See docs/MACOS_BACKEND_PLAN.md.
//
// ⚠️ WRITTEN FROM APPLE'S DOCUMENTED FSEvents API, WITH ZERO BUILD VERIFICATION OF
// ANY KIND. Unlike the Windows USN backend (usn_windows.go), which at least
// cross-compiles cleanly from Linux, this file cannot even be *compiled* here: it
// requires CGO_ENABLED=1 plus the actual macOS SDK (Xcode Command Line Tools),
// neither of which exists on a non-Mac host. It has never been run through a Go
// compiler at all. Treat every struct layout, constant, and cgo pattern below as a
// first draft to verify line-by-line on a Mac — not as reviewed code. Start with
// docs/MACOS_BACKEND_PLAN.md step 1 (the standalone fsevents-smoke command) rather
// than trusting this file end-to-end.
//
// This layer is NOT wired into SelectSource (source_select_darwin.go still returns
// the portable backend) — nothing calls it today.

// fsEventsSinceNow mirrors kFSEventStreamEventIdSinceNow (defined in FSEvents.h as
// (FSEventStreamEventId)0xFFFFFFFFFFFFFFFFULL — i.e. UINT64_MAX). Passed as `since`
// to start a stream with no historical replay.
const fsEventsSinceNow uint64 = ^uint64(0)

// Per-event FSEventStreamEventFlags bits used by source_darwin.go (FSEvents.h).
// Only kFSEventStreamCreateFlagFileEvents-mode flags are listed — mirrored as plain
// Go constants (not read via C.kFSEventStream…) because they're needed outside this
// cgo-only file, in plain uint32 values delivered through the callback.
const (
	flagMustScanSubDirs  uint32 = 0x00000001
	flagUserDropped      uint32 = 0x00000002
	flagKernelDropped    uint32 = 0x00000004
	flagHistoryDone      uint32 = 0x00000010
	flagRootChanged      uint32 = 0x00000020
	flagItemCreated      uint32 = 0x00000100
	flagItemRemoved      uint32 = 0x00000200
	flagItemInodeMetaMod uint32 = 0x00000400
	flagItemRenamed      uint32 = 0x00000800
	flagItemModified     uint32 = 0x00001000
	flagItemXattrMod     uint32 = 0x00008000
	flagItemIsFile       uint32 = 0x00010000
	flagItemIsDir        uint32 = 0x00020000
	flagItemIsSymlink    uint32 = 0x00040000
)

// fsEvent is one raw FSEvents record, decoded from the C callback's parallel arrays
// into something ordinary Go code (source_darwin.go, cmd/fsevents-smoke) can use.
type fsEvent struct {
	Path  string
	Flags uint32
	ID    uint64
}

// fsEventSink receives a batch of events exactly as FSEvents delivered them (one
// callback invocation = one batch; event IDs within a batch are non-decreasing).
// darwinWatch (source_darwin.go) is the real sink; debugSink (below) is a minimal
// one for cmd/fsevents-smoke.
type fsEventSink interface {
	onBatch(events []fsEvent)
}

// fsEventStream is one running FSEventStreamRef + its dispatch queue + the
// cgo.Handle keeping the sink reachable from the C callback. Every field is opaque
// outside this file; callers only get a *fsEventStream from newFSEventStream and
// call Close/LatestEventID on it.
type fsEventStream struct {
	ref    C.FSEventStreamRef
	queue  C.dispatch_queue_t
	handle cgo.Handle
}

// newFSEventStream opens one FSEvents stream covering paths, delivering batches to
// sink. since is fsEventsSinceNow for a fresh start, or a previously-persisted
// event ID to replay history since then (see fsEventsCursorStore in
// source_darwin.go) — FSEvents keeps a real per-volume event log, so this is
// genuine catch-up, not a re-walk, mirroring the Windows USN journal cursor.
//
// latency batches rapid changes together before delivery (FSEventStreamCreate's
// `latency` parameter, in seconds) — a few hundred milliseconds is a reasonable
// starting point; tune once real event volume can be observed on a Mac.
func newFSEventStream(paths []string, since uint64, latency time.Duration, sink fsEventSink) (*fsEventStream, error) {
	if len(paths) == 0 {
		return nil, errors.New("fsevents: no paths to watch")
	}

	cPaths := make([]*C.char, len(paths))
	for i, p := range paths {
		cPaths[i] = C.CString(p)
	}
	defer func() {
		for _, p := range cPaths {
			C.free(unsafe.Pointer(p))
		}
	}()

	h := cgo.NewHandle(sink)
	ref := C.fw_create_stream(
		(**C.char)(unsafe.Pointer(&cPaths[0])), C.int(len(cPaths)),
		C.uintptr_t(h),
		C.FSEventStreamEventId(since),
		C.CFTimeInterval(latency.Seconds()),
	)
	if ref == nil {
		h.Delete()
		return nil, errors.New("fsevents: FSEventStreamCreate returned NULL")
	}

	q := C.fw_start(ref)
	return &fsEventStream{ref: ref, queue: q, handle: h}, nil
}

// Close stops and releases the stream. Safe to call once; the sink stops receiving
// batches once this returns (the dispatch queue's last scheduled callback has run).
func (s *fsEventStream) Close() {
	C.fw_stop_and_release(s.ref, s.queue)
	s.handle.Delete()
}

// LatestEventID returns the stream's most recently processed event ID — used to
// seed the cursor on close/persist, and via FSEventStreamGetLatestEventId which
// works even before any event has been delivered (it returns `since`).
func (s *fsEventStream) LatestEventID() uint64 {
	return uint64(C.FSEventStreamGetLatestEventId(s.ref))
}

// fsEventsUUIDForDevice returns the FSEvents log identity for the volume containing
// dev (a dev_t from syscall.Stat_t.Dev — see deviceOf in source_darwin.go), or
// ok=false if the volume isn't FSEvents-eligible (network mounts, some external
// drives) or has no event log. The UUID changes if that volume's event history was
// reset (reformat, event log purge) — Apple's documented way to tell whether a
// persisted "since" event ID is still safe to resume from, or must be discarded in
// favor of starting fresh. See resumeSince in source_darwin.go.
func fsEventsUUIDForDevice(dev uint64) (string, bool) {
	cs := C.fw_fsevents_uuid_for_device(C.int32_t(dev))
	if cs == nil {
		return "", false
	}
	defer C.free(unsafe.Pointer(cs))
	return C.GoString(cs), true
}

// debugSink adapts a plain callback func to fsEventSink, for DebugWatchFSEvents.
type debugSink struct {
	fn func(path string, flags uint32, eventID uint64)
}

func (d *debugSink) onBatch(events []fsEvent) {
	for _, e := range events {
		d.fn(e.Path, e.Flags, e.ID)
	}
}

// DebugWatchFSEvents is a minimal, standalone entry point into the raw FSEvents
// layer, exported for cmd/fsevents-smoke — the first thing to run when debugging
// this backend on a real Mac (docs/MACOS_BACKEND_PLAN.md step 1), since it
// exercises exactly this file's cgo plumbing without any of source_darwin.go's
// Change-mapping/cursor/exclude logic in the way. Not used by the indexer itself.
func DebugWatchFSEvents(paths []string, onEvent func(path string, flags uint32, eventID uint64)) (stop func(), err error) {
	sink := &debugSink{fn: onEvent}
	s, err := newFSEventStream(paths, fsEventsSinceNow, 300*time.Millisecond, sink)
	if err != nil {
		return nil, err
	}
	return s.Close, nil
}

//export goFSEventsCallback
func goFSEventsCallback(streamRef C.ConstFSEventStreamRef, info unsafe.Pointer, numEvents C.size_t,
	eventPaths **C.char, eventFlags *C.FSEventStreamEventFlags, eventIds *C.FSEventStreamEventId) {
	n := int(numEvents)
	if n == 0 || info == nil {
		return
	}
	sink, ok := cgo.Handle(uintptr(info)).Value().(fsEventSink)
	if !ok || sink == nil {
		return
	}

	paths := unsafe.Slice(eventPaths, n)
	flags := unsafe.Slice(eventFlags, n)
	ids := unsafe.Slice(eventIds, n)

	batch := make([]fsEvent, n)
	for i := 0; i < n; i++ {
		batch[i] = fsEvent{
			Path:  C.GoString(paths[i]),
			Flags: uint32(flags[i]),
			ID:    uint64(ids[i]),
		}
	}
	sink.onBatch(batch)
}
