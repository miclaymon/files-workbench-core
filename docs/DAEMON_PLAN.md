# OS-managed daemon (Phase 4) — status & per-OS guide

**What this is:** promoting `fw-indexer` from an *app-child* (spawned and killed by
the core data server) to an *OS-managed daemon* (systemd `--user` / launchd
LaunchAgent / Windows service) so the search index stays warm across app restarts
and while the app is closed, and is shared by every app instance.

**What's built:** the full scaffolding for all three OSes, plus the core-side
connect-or-spawn logic that ties the two lifecycles together. Installing the daemon
is opt-in — a stock checkout keeps using the app-child model with zero setup.

---

## Verification status (read this first)

| Piece | Cross-compiles | Runtime-tested |
|---|---|---|
| `service` pkg — Linux (systemd) | ✅ | ✅ **fully tested** on the Linux dev box: install → running → serves name+content search → adopted by core → survives core exit → clean uninstall |
| `service` pkg — macOS (launchd) | ✅ | ❌ launchctl semantics unverified |
| `service` pkg — Windows (SCM) | ✅ (amd64+arm64) | ❌ SCM create/start/stop/delete, registry env, svc.Run handler all unverified |
| core connect-or-spawn (`indexer_proxy.go`) | ✅ | ✅ tested — core logs "adopting", proxies to the daemon, spawns no child, leaves it running on exit |
| `cmd/fw-indexer` subcommands + run refactor | ✅ | ✅ (Linux) install/uninstall/service-status exercised |

None of this needs cgo (unlike the Phase-3 FSEvents backend): systemd/launchd are
file writes + `systemctl`/`launchctl` subprocess calls, and Windows uses the pure-Go
`golang.org/x/sys/windows/svc` + `.../mgr` + `.../registry`. So everything
compile-checks on every target from Linux; only the launchd/SCM *runtime* behavior
is unverified.

> Note: `GOOS=darwin go build ./cmd/fw-indexer/...` currently fails — but on the
> Phase-3 **FSEvents** backend (`indexer/source_darwin.go`), not on any Phase-4 code.
> The `service` package builds clean for darwin standalone
> (`go build ./service/...`). Once the FSEvents backend is resolved on a Mac
> (`docs/MACOS_BACKEND_PLAN.md`), the darwin `cmd` build comes back and the launchd
> daemon can be exercised.

---

## Architecture

### Two lifecycles, one client

```
App-child (Phases 1–3, default)          OS daemon (Phase 4, opt-in)
─────────────────────────────           ───────────────────────────
core spawns fw-indexer,                  systemd/launchd/SCM owns fw-indexer;
supervises, kills on quit.               it outlives the app.
Index warm only while app runs.          Index warm across restarts + logins.
        │                                        │
        └──────────────┬─────────────────────────┘
                       ▼
        core `startIndexManager` (indexer_proxy.go):
          probe GET FW_INDEX_ADDR/health
            ├─ 200? → ADOPT: proxy only; never supervise or kill
            └─ no?  → SPAWN a child + supervise (today's behavior)
```

The adopt-vs-spawn decision is made once at core startup. Because an OS daemon
carries its own `FW_INDEX_ROOTS` in its service definition, the health probe runs
*before* core's own roots gate — core can front a running daemon even when core
itself wasn't given `FW_INDEX_ROOTS`.

### Config flows through the service definition

The daemon can't get its config from core (core may not be running when the OS
starts it), so `fw-indexer install` bakes everything into the OS service
definition's environment: `FW_INDEX_ROOTS`, `FW_INDEX_ADDR`, `FW_INDEX_DB`,
`FW_DATA_DIR`, and any `FW_INDEX_CONTENT{,_BUDGET}` / `FW_INDEX_NATIVE` present in
the installer's environment. `service.Config.Env` is the single source of truth;
each OS backend translates it (systemd `Environment=`, launchd
`EnvironmentVariables`, Windows registry `Environment` REG_MULTI_SZ).

### Commands

```
fw-indexer install [--roots R --addr A --db D]   register + start the daemon
fw-indexer uninstall                             stop + deregister
fw-indexer service-status                        report install/run state
fw-indexer [run flags]                           run in the foreground (default)
```

`install` reuses the run flags so the daemon runs with exactly the config you'd
have run interactively.

---

## Per-OS notes

### Linux — systemd `--user` ✅ tested

- Unit written to `~/.config/systemd/user/fw-indexer.service`, then
  `systemctl --user daemon-reload` + `enable --now`.
- Runs **as the user, no root**. Idle I/O + `Nice=19` keep it invisible;
  `Restart=on-failure` replaces core's supervisor.
- **Logged out warmth:** a `--user` service stops when the user's last session ends
  *unless lingering is enabled* — `loginctl enable-linger $USER`. Left to the user
  (it's a privilege/policy choice); surfaced in `service-status` output.
- No further work needed here beyond wiring it into the app's UI (see "Remaining").

### macOS — launchd LaunchAgent ⚠️ untested

- Plist written to `~/Library/LaunchAgents/com.filesworkbench.fw-indexer.plist`,
  loaded with `launchctl load -w`.
- **Most likely thing to need adjustment:** `load -w` / `unload -w` are the
  broadly-compatible spelling but are deprecated on recent macOS in favor of
  `launchctl bootstrap gui/<uid> <plist>` / `launchctl bootout gui/<uid>/<label>`.
  If `load -w` misbehaves, switch `launchdManager.Install`/`Uninstall` to the
  bootstrap form (uid via `os.Getuid()`).
- `ProcessType=Background` requests idle scheduling; `KeepAlive{SuccessfulExit:false}`
  restarts only on crash.
- Runs as the user, no root. Validate: install, `launchctl list com.filesworkbench.fw-indexer`
  shows a PID, `curl FW_INDEX_ADDR/health`, reboot/logout-login to confirm warmth,
  uninstall leaves nothing behind.

### Windows — Service Control Manager ⚠️ untested

- Registered via `mgr.Connect().CreateService(...)`; per-service env written to
  `HKLM\SYSTEM\CurrentControlSet\Services\fw-indexer\Environment` (REG_MULTI_SZ),
  which the SCM merges into the process environment at launch.
- **Elevation:** `install`/`uninstall` need an **elevated (admin)** process — SCM
  calls fail access-denied otherwise. The service's default account is
  **LocalSystem**, which is elevated — deliberately, because the Phase-3 USN/MFT
  backend needs a raw volume handle. To run unelevated (portable backend only), set
  `ServiceStartName` to the user account in `mgr.Config` and install with
  `FW_INDEX_NATIVE=0`.
- **svc.Run handler:** when the SCM launches the binary, `svc.IsWindowsService()` is
  true and `run_windows.go` runs the indexer under the SCM control protocol
  (stop/shutdown → context cancel). This is the fiddliest untested piece — validate
  that Stop actually stops it within the SCM's timeout and that Interrogate replies.
- Validate elevated: `fw-indexer install --roots C:\Users\me`, check
  `services.msc`/`sc query fw-indexer`, `curl` the addr, reboot to confirm it
  auto-starts, `fw-indexer uninstall` removes the service + registry key.

---

## Cross-cutting concerns (design decisions & open items)

- **Update coordination (open).** When the app updates, the daemon binary on disk
  changes but the *running* daemon is the old one until restarted. The app should,
  on startup, compare its bundled `fw-indexer` version against the running daemon's
  (add a `version` field to `/status`) and re-`install` (which stops+recreates) on
  mismatch. Not implemented — the app-child model sidesteps it, so it's only needed
  once the daemon ships.
- **Stale-daemon adoption (accepted limitation).** Adoption is a startup decision; if
  the adopted daemon later dies and the OS *doesn't* restart it, core won't fall back
  to spawning a child until its next start (the proxy just 503s meanwhile — the
  existing `writeIndexUnavailable` path). A periodic re-probe could re-spawn, but it
  risks racing the OS service manager's own restart. Left as-is; documented.
- **Config drift (accepted limitation).** If core's `FW_INDEX_ROOTS` disagrees with
  the daemon's baked-in roots, the daemon's win (core only proxies). The app's daemon
  UI should show the daemon's actual roots (from `/status`), not core's env.
- **Port vs. socket.** The daemon still listens on a localhost TCP port
  (`FW_INDEX_ADDR`). INDEX.md's open item #4 (move to a Unix socket / named pipe) is
  more pressing once the daemon is long-lived and content-indexing — a persistent
  localhost port is a wider surface than an app-child one. Not addressed here.

---

## Remaining to finish Phase 4

1. **Validate macOS + Windows** on real hardware (checklists above).
2. **App UI** — a Settings toggle "Run the search index as a background service"
   that shells out to `fw-indexer install`/`uninstall` (elevating on Windows) and
   shows `service-status`. Ties into the roots-as-preference follow-up (the daemon's
   roots should come from that preference).
3. **Version handshake** for update coordination (above).
4. **Socket transport** (INDEX.md open item #4) before the daemon is on by default.
