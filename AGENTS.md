# Agent Guide — @files-workbench/core

Instructions for AI coding agents (Claude Code, etc.) working in this repository.

This package is the **data layer** of [Files Workbench](https://github.com/miclaymon/files-workbench)
(see that repo's `PLAN.md` for the multi-package refactor map): the Go HTTP
backend and the JS client library that talks to it, versioned together so the
wire protocol never drifts. The app consumes it via a local install
(`npm install ../../files-workbench-core` → a `file:` symlink) and runs/bundles
the server from this checkout.

## Layout

- **`server/`** — the Go backend (stdlib `net/http`, no framework; Go 1.23).
  One process, two independent servers so slow reads never block writes:

  | Server | Port (env) | Content |
  |---|---|---|
  | Data | 8001 (`PORT`) | read-only GETs — listing, stat, media, icons, preferences, plugin manifest/artifacts |
  | Control | 8002 (`CONTROL_PORT`) | mutating POST/PUT/DELETE — rename, move, copy, delete, trash, compress, prefs write, plugin install/rpc-write |

  All routes under `/_api/v1/`; permissive CORS on every response. Do not
  register write routes on the data mux or vice versa (`main.go` →
  `registerDataRoutes` / `registerControlRoutes`).

- **`src/`** — the JS client library (raw ESM, compiled by the consuming app's
  bundler; exported flat from `src/index.js` — export names are load-bearing):
  `api-config.js` (`API_BASE`/`CONTROL_BASE` from `VITE_API_BASE`/`VITE_CONTROL_BASE`
  at build time), `fs-api.js`, `explorer-api.js`, `explorer-roots.js`,
  `sw-queue.js`, `plugin-rpc.js`, `plugins-api.js`, `perf-log.js`.

## Key server files

| File | Contents |
|---|---|
| `main.go` | route registration, CORS, dual-server startup, and the `FW_*` path-root resolution (below) |
| `fs.go` | filesystem CRUD, trash/delete (+ elevated variants), compress/decompress, open-with/terminal, `dir_size` (TTL cache + invalidation) |
| `archive.go` | archive listing as virtual directories (ZIP/TAR*/7Z/RAR), tool-capability probing |
| `permissions.go` | protected-path blocking + elevation detection |
| `explorer.go` | tree roots/home/drives/lazy subtree |
| `media.go` / `thumbnail.go` | media serving, metadata, thumbnails (x/image + ffmpeg), disk cache |
| `icons.go` / `plugins.go` | icon-pack loading (`FW_CONFIG_DIR/plugins/*/plugin.json`) + `/icons/manifest` + `/icons/svg` |
| `preferences.go` | preferences read/write/schema (schema+defaults from config dir, user file in data dir) |
| `plugin_host.go` | the sandboxed WASM server-plugin host (extism/wazero): permissioned host functions (`exec:<bin>`, `fs:read`, …), `POST /_api/v1/plugins/{id}/rpc` |
| `plugins_serve.go` / `plugins_install.go` | runtime plugin manifest/artifact serving; third-party install/uninstall/enable with server-side hash recompute |
| `customization.go` | lossless `.directory`/`desktop.ini` read/write + pinned items |
| `blacklist.go` | path-exclusion rules from `FW_BLACKLIST` |

## Path roots (`FW_*` env contract)

The server resolves every disk root from env vars, falling back to
package-relative paths for standalone runs (`main.go`):

| Var | Holds | Fallback |
|---|---|---|
| `FW_CONFIG_DIR` | read-only config (pref schema/defaults, icon-pack plugins) | `<pkg>/config` |
| `FW_DATA_DIR` | writable user data (`user-preferences.json`, third-party `plugins/`) | `<pkg>/config` |
| `FW_PLUGINS_DIR` | built first-party plugin artifacts | `<pkg>/.fw/plugins` |
| `FW_LOGS_DIR` | logs | `<pkg>/server/logs` |
| `FW_BLACKLIST` | protection rules | `<pkg>/server/blacklist.yaml` |

**A host app always sets these.** Files Workbench points them at its own tree in
`dev:server` (repo root `config/` + `.fw/plugins`) and from Electron's main
process in packaged builds (app resources + `userData`). Any new file the server
reads/writes must go through one of these roots — never assume a repo layout.

## Gotchas (protocol-level — the app relies on these)

- **Icon SVG 404s must return `Content-Type: image/svg+xml`** — a non-image 404
  triggers Chrome's ORB and suppresses the client's `<img>` `@error` fallback.
- **`kind: "archive"`** in listings drives client navigation (`path + '::'`);
  archive extensions are classified server-side.
- **`dir_size` is a separate endpoint by design** (TTL cache + per-cell client
  updates) — don't fold sizes into `list_dir`.
- **`.ts` MIME**: detection may say `video/mp2t`; clients check extension first.
  Keep it that way when touching MIME logic.
- **Response cache**: listings may be served from the SQLite/TTL cache
  (preference-controlled, default 30 s) — write handlers must invalidate what
  they change (`invalidateDirSize`, cache eviction).
- **The service worker script lives in the app** (`client/public/sw.js`, root
  scope requirement). Its `ENDPOINTS` map mirrors the one in `src/sw-queue.js`
  — **adding a write endpoint means updating both**.
- `customization.go` reads bypass the blacklist intentionally (internal
  enrichment, not listing exposure).

## Conventions

- Go: stdlib only where practical; handlers return JSON via the `jsonOK`/`jsonErr`
  helpers; structured 403 (`requiresElevation`) and 422 (`missingTool`) responses
  are part of the client contract.
- JS: plain ESM + JSDoc; reads via `API_BASE`, writes via `CONTROL_BASE`; every
  new module's exports get re-exported from `src/index.js`.
- After adding a Go import: `cd server && go mod tidy`.

## Verifying changes

```bash
cd server && go build ./...        # compile
# run against the app's tree the way its dev:server does:
cd ../../files-workbench-app && npm run dev:server
# client side:
cd client && npx vite build && npm run dev
```

`curl http://localhost:8001/health` checks the data server; exercise listings,
media, a write op (rename), and the plugin endpoints from the app UI.
