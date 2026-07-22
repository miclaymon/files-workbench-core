# macOS backend implementation plan (Phase 3)

**Audience:** whoever picks this up next — human or AI agent — on a real Mac. This
doc assumes no memory of how it got here; it's meant to be self-contained.

**Status at handoff:** the FSEvents backend is **written but has never been
compiled**. Not "compile-checked and untested" like the Windows USN backend
(`usn_windows.go`) — genuinely never run through any Go/C toolchain, because that
toolchain doesn't exist on the machine that wrote it (see "Why this couldn't be
verified" below). Treat every struct layout, constant, and cgo pattern in
`server/indexer/fsevents_darwin.go` and `server/indexer/source_darwin.go` as a
first draft to check line-by-line against real behavior, not as reviewed code.
It is **not wired up** — `SelectSource` (`source_select_darwin.go`) still returns
the portable backend unconditionally. Nothing calls `newDarwinSource` today.

---

## Why this couldn't be verified (and why Windows was different)

The Windows USN backend reaches the change journal through raw Win32 syscalls
(`DeviceIoControl` etc.) via `golang.org/x/sys/windows` — pure Go, no C compiler
involved. Go's own toolchain can cross-compile `windows/amd64` and `windows/arm64`
from Linux with `CGO_ENABLED=0` and produce a real, correct binary, so
cross-compiling it was a meaningful verification step.

**FSEvents isn't a syscall.** Unlike `kqueue` — a real BSD syscall, which
`fsnotify` already uses on macOS today via `golang.org/x/sys/unix`, no cgo needed,
and which is exactly what the *current* portable backend falls back to — FSEvents
is a CoreServices/CoreFoundation *framework* API (`FSEventStreamCreate` and
friends). There is no syscall-level equivalent; reaching it requires actual C
interop, i.e. `cgo` with `#cgo LDFLAGS: -framework CoreServices`.

`cgo` needs `CGO_ENABLED=1` **and** a C compiler that can produce Mach-O binaries
**and** the real macOS SDK headers/stub libraries for CoreServices/CoreFoundation.
Those only ship inside Xcode / Xcode Command Line Tools, which Apple distributes
for installation on macOS itself. There's no official way to get that toolchain on
Linux (the unofficial `osxcross` workaround exists but is unmaintained/fragile and
legally murky about reusing Apple's SDK off Apple hardware — not something to wire
into this project's build). So cross-compiling this file was never on the table;
confirmed empirically — see "What was actually checked" below.

**Practical upshot:** don't trust anything here until you've compiled it. Start
with step 1, not by wiring `SelectSource` straight to `newDarwinSource`.

---

## What was actually checked (and what wasn't)

From the Linux machine that wrote this:
- ✅ `gofmt -l` on the new files — confirms they're syntactically valid Go (gofmt
  treats the whole cgo preamble as an opaque comment, so this says nothing about
  the C code inside it).
- ✅ The rest of the package (Linux build, `go vet`, `go test ./indexer/...`,
  Windows amd64+arm64 cross-compile) still passes — the darwin-tagged files are
  build-tag-excluded from those, so this only proves the refactor that shared
  `statEntry`/`underAny` between the Windows and macOS backends
  (`source_common.go`) didn't break anything real.
- ❌ `GOOS=darwin GOARCH=amd64 go build ./...` — **fails** with `undefined:
  newFSEventStream` and friends. `source_darwin.go` (plain Go) compiles fine
  standalone, but `fsevents_darwin.go` (the file with `import "C"`) gets silently
  excluded from a darwin cross-build on this machine (no darwin-capable C
  toolchain configured), so the package fails to link the two together. This is
  expected, not a bug to fix here — it's the concrete proof that **nothing in
  `fsevents_darwin.go` has ever been type-checked, let alone compiled or run.**
  (Before this backend existed, `GOOS=darwin go build ./cmd/fw-indexer/...`
  *did* pass, because macOS had no cgo files at all yet — just the portable
  fallback. That check is gone now; it can't come back until this lands on a Mac.)

---

## File map

| File | Mirrors (Windows) | Status |
|---|---|---|
| `server/indexer/fsevents_darwin.go` | `usn_windows.go` | Low-level cgo layer: stream create/start/stop, the exported C callback, event-flag constants, per-volume UUID lookup. **Never compiled.** |
| `server/indexer/source_darwin.go` | `source_windows.go` | `darwinSource` (`Source` impl): `Walk` (portable), `Watch` (FSEvents + cursor + fallback), event→`Change` mapping, cursor persistence. **Never compiled.** |
| `server/indexer/source_common.go` | — | `statEntry`/`underAny`, extracted out of `source_windows.go` so both native backends share them. No build tag — **this part is real, verified Go**, exercised by the Linux/Windows builds today. |
| `server/cmd/fsevents-smoke/main.go` | — (new, macOS-only) | Standalone CLI that drives `indexer.DebugWatchFSEvents` and prints raw events. The first thing to run — see step 1. |
| `server/indexer/source_select_darwin.go` | `source_select_windows.go` | Still returns the portable backend. Update this last (step 5). |

---

## Step-by-step plan

### 1. Get a compiler, then compile the smoke test in isolation

```
xcode-select --install        # if `clang --version` doesn't already work
cd files-workbench-core/server
go build ./cmd/fsevents-smoke/...
```

This is deliberately the smallest possible surface: it only exercises
`fsevents_darwin.go` (stream create/start/callback/stop), not any of
`source_darwin.go`'s Change-mapping or cursor logic. Fix whatever the compiler
finds first — expect real errors here, this is genuinely first-contact. Likely
candidates, roughly in order of how suspicious they are:

- **The `//export` + cgo preamble interaction.** `goFSEventsCallback` is declared
  `extern` in the preamble and defined as an exported Go function in the same
  file. This is a real, common cgo pattern, but double-check the generated
  `_cgo_export.h` matches what the preamble expects — if cgo complains about a
  redeclaration or signature mismatch, the usual fix is moving the `//export`
  function to its own file.
- **`FSEventStreamContext.info` as `uintptr_t`.** The real Apple struct field is
  `void *info`. The code passes it through as `uintptr_t` end-to-end specifically
  to avoid ever constructing `unsafe.Pointer(uintptr(x))` in Go (a pattern `go
  vet`'s unsafeptr check flags, and one of the easier ways to get cgo's pointer
  rules subtly wrong) — the actual `void*` cast happens in C, inside
  `fw_create_stream`. Verify this compiles and that the handle round-trips
  correctly (the smoke test's first successful callback is the proof).
- **`dispatch_release(q)`.** Whether this is still correct / necessary on modern
  SDKs (vs. ARC-managed dispatch objects) is exactly the kind of thing that was
  impossible to check without a real SDK version in hand. If `dispatch_release`
  doesn't exist or double-frees, that's a one-line fix.
- **Enum/macro access in the preamble** (`kFSEventStreamCreateFlagFileEvents`,
  `kCFTypeArrayCallBacks`, `kCFStringEncodingUTF8`, etc.) — these are referenced
  directly from C code in the preamble (not re-derived as Go constants), so if
  they compile at all they're correct by construction; the risk here is a missing
  `#include` rather than a wrong value.

Once it builds:

```
./fsevents-smoke ~/Desktop/fsevents-test    # or any scratch directory
```

In another terminal, `touch`/`echo >`/`mv`/`rm` files under that directory and
confirm events print with sane paths and flag names. This validates the entire
low-level layer — stream lifecycle, the C↔Go callback bridge, path/flag
marshaling — independent of everything else. **Don't move to step 2 until this
works.**

### 2. Compile the rest of the package

```
go build ./...
go vet ./...
```

`source_darwin.go` has never been type-checked at all (see "What was actually
checked" above) — everything from field names to control flow is unverified here,
not just the cgo bits. Expect to actually read and fix this file, not just paper
over compiler errors.

### 3. Validate cursor persistence / cross-restart catch-up

This is the feature that makes the native backend worth having over the portable
fsnotify fallback (besides not hitting the OS watch-budget on huge trees) — verify
it actually works:

1. Run the indexer against a test root, make some changes, let it process them.
2. Kill it (don't let it clean up) — check
   `<FW_DATA_DIR>/index/fsevents-cursor.json` was written with a non-zero
   `latestEventId` and a `deviceUuids` entry for the test root's volume.
3. Make more changes *while the indexer is down*.
4. Relaunch it. Confirm (via logs — `resumeSince` logs whether it resumed or
   started fresh) that it resumed from the cursor, and that the changes made while
   it was down show up **without a full re-walk**.
5. Separately, confirm the safety side: hand-edit the cursor file with a bogus
   `deviceUuids` entry (simulating a volume whose FSEvents log got reset) and
   confirm it falls back to `fsEventsSinceNow` instead of trying to resume from a
   now-invalid ID.

### 4. Validate the coalesce/overflow path

Trigger `MustScanSubDirs`/`UserDropped`/`KernelDropped` — the reliable way is a
very high-velocity burst of changes (e.g. `git clone` a large repo, or a tight loop
creating/deleting thousands of small files) under a watched root. Confirm
`darwinWatch.rescan` fires and the affected subtree ends up correctly indexed
afterward. **Known gap, by design, documented in `onBatch`'s comment:** the rescan
only re-adds what `Walk` still finds — it does not detect deletions that happened
during the gap, since `Source` has no store access to diff against. If this proves
to matter in practice, the fix belongs at the service layer (`service.go`), which
does have the store, not in `Source`.

### 5. Wire it up

Once 1–4 hold up, flip `source_select_darwin.go`:

```go
func SelectSource(exclude ExcludeFunc, log func(string)) Source {
	if os.Getenv("FW_INDEX_NATIVE") == "0" {
		log("macos: native FSEvents backend disabled (FW_INDEX_NATIVE=0), using portable")
		return NewPortableSource(exclude, log)
	}
	return newDarwinSource(exclude, log)
}
```

(Exactly mirrors `source_select_windows.go` — keep the escape hatch; it's the
safety net for whatever step 1–4 didn't catch.) Then run the existing Go test
suite (`go test ./indexer/...`) on macOS — it's almost entirely OS-agnostic
already (it exercises `Source` through the interface), so it should mostly just
pass, but this is also the point to add a `source_darwin_test.go` that drives
`newDarwinSource` directly against a real temp directory: create/modify/rename/
delete files, assert the right `Change`s come out the other end. That test can't
be written on Linux (build-tag excluded) — write it on the Mac alongside the
debugging, using `store_test.go`/`watch_test.go` in this package as the pattern
to follow for style.

### 6. Update the docs

`docs/INDEX.md`'s Phase 3 paragraph and `AGENTS.md`'s optional-tools note both
currently describe Windows as the only native backend landed — update them the
same way the Windows work did, including dropping the "written but not wired"
framing once step 5 is done. This file (`MACOS_BACKEND_PLAN.md`) can be deleted
once the backend is wired, tested, and its remaining caveats folded into
`INDEX.md` proper — it's a scratch-to-permanent handoff doc, not meant to be
maintained forever.

---

## Capabilities that must be supported

`darwinSource.Caps()` currently declares:

```go
SourceCaps{Realtime: true, Content: false, JournalCatchup: true}
```

- **Realtime: true** — FSEvents delivers live events (with `latency` batching).
- **Content: false** — matches every other backend; content extraction is a
  separate, OS-agnostic pipeline (`content.go`/`scanner.go`) layered on top of
  whatever `Source` supplies, not something `Source` itself provides.
- **JournalCatchup: true** — this is the whole point of doing FSEvents instead of
  staying on the portable kqueue-based fsnotify: FSEvents keeps a real per-volume
  event log, so a persisted event ID gives genuine cross-restart catch-up (step 3),
  the same guarantee the Windows USN journal gives. If step 3 can't be made to
  work reliably, this must become `false` and the persisted-cursor code path
  should be removed rather than left in a half-working state — don't ship
  `JournalCatchup: true` without step 3 actually holding up.

`Watch` must, at minimum:
- Emit `Change{Op: ChangeAdded, ...}` for new files/dirs, `ChangeModified` for
  content/metadata changes, `ChangeRemoved` for deletions — for **every path
  under any watched root**, not just the roots themselves.
- Respect `exclude` (the `ExcludeFunc` built from `FW_BLACKLIST`) — never emit
  changes for excluded paths.
- Never block for long in the FSEvents callback path (see `emit`'s comment in
  `source_darwin.go` — it currently drops-and-logs on backpressure rather than
  blocking, since blocking risks stalling FSEvents' own dispatch queue).
- Fall back to the portable watcher on any setup failure, so the indexer never
  simply stops working because the native path had a problem.

`Walk` only needs to match the portable walker's behavior (it *is* the portable
walker, delegated to `d.fallback.Walk`) — no native acceleration is planned for
the initial index (see "Initial index" below).

---

## Initial index: why Spotlight wasn't attempted

The design doc's Phase 3 line mentions "macOS Spotlight" alongside FSEvents — worth
being explicit that those are two different things, and only FSEvents was
attempted here.

**FSEvents** is the live-watch mechanism — the direct analogue of the Windows USN
journal, and what this whole doc is about.

**Spotlight** (`NSMetadataQuery`/`mdfind`) is Apple's own system-wide content
index, which could theoretically stand in for something like Windows' MFT
enumeration — a way to get a near-instant *initial* catalog instead of a cold
`filepath.WalkDir`. It wasn't attempted, for reasons worth recording so nobody
re-derives them:

- It's cgo-only too (same toolchain problem as FSEvents), so it wouldn't have
  been any more verifiable from Linux.
- Users can and do disable Spotlight indexing per-volume (Privacy pane), so it
  can't be relied on as the *only* path to an initial index — a portable-walk
  fallback would still be mandatory, same as here.
- Spotlight doesn't index paths the user has excluded from it (a separate
  exclusion list from `FW_BLACKLIST`), so reconciling "what Spotlight has" against
  "what we actually want indexed" adds a second exclusion model to reason about.
- `NSMetadataQuery` is inherently asynchronous and run-loop-based (you kick off a
  query, wait for `NSMetadataQueryDidFinishGatheringNotification`), which is a
  materially different integration shape than the synchronous `Walk(ctx, root,
  emit)` the `Source` interface expects — it would need its own adapter, not a
  small addition to what's here.

None of that rules it out — if the portable walk's cold-start time on a large
macOS home directory proves to be a real problem in practice, Spotlight is the
next thing to evaluate — but it's a separate, larger piece of work than finishing
the FSEvents watcher, and shouldn't block wiring up what's already written here.

---

## Known gaps (carried over from the design, not new)

- **Rescan-on-overflow doesn't detect deletions** during the gap (step 4) — needs
  store access to fix properly, which `Source` intentionally doesn't have.
- **`emit`'s drop-and-log backpressure policy is unverified under load** — the
  right fix if drops prove costly (decoupling the callback from channel
  consumption speed via an internal queue + separate draining goroutine) is noted
  in the code but not implemented, since there's no way to know if it's needed
  without real event volume to observe.
- **No MFT-equivalent initial-index acceleration** (see "Initial index" above) —
  `Walk` is the portable walk, same as Windows today.
