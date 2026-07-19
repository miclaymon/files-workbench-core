# @files-workbench/core

The **data layer** of [Files Workbench](https://github.com/miclaymon/files-workbench):
the Go HTTP backend and the JS client library that talks to it, versioned together
so the protocol never drifts.

## Layout

- **`server/`** — the Go backend (stdlib `net/http`, Go 1.23). One process, two
  servers: a read-only **data** server (port 8001, `PORT`) and a mutating
  **control** server (port 8002, `CONTROL_PORT`), all routes under `/_api/v1/`.
  Functional areas: filesystem CRUD + trash/compress/extract, archive browsing,
  path protection/elevation, explorer tree, media (thumbnails via
  `golang.org/x/image` + ffmpeg), icon packs, preferences, directory
  customization (`.directory`/`desktop.ini`), performance ingestion, and the
  plugin subsystem (artifact serving + install + the sandboxed extism/wazero
  WASM host for plugin backends).
- **`src/`** — the JS client library, imported as `'@files-workbench/core'`:
  `api-config` (`API_BASE`/`CONTROL_BASE` from `VITE_API_BASE`/`VITE_CONTROL_BASE`),
  `fs-api` (reads + non-queued ops), `explorer-api` + `explorer-roots`,
  `sw-queue` (the service-worker operations bridge; the SW script itself lives in
  the host app's `public/`), `plugin-rpc` (WASM-backend RPC), `plugins-api`
  (manifest/artifact/install/registry), `perf-log`.

## Running the server

The server resolves its roots from env vars, falling back to package-relative
paths (see `server/main.go`):

| Var | Holds |
|---|---|
| `FW_CONFIG_DIR` | read-only config: preference schema/defaults, icon-pack plugins |
| `FW_DATA_DIR` | writable user data (`user-preferences.json`, third-party `plugins/`) |
| `FW_PLUGINS_DIR` | built first-party plugin artifacts |
| `FW_LOGS_DIR`, `FW_BLACKLIST` | logs dir; path-protection rules |

A host app points these at its own tree — Files Workbench does so in its
`dev:server` script and from Electron's main process in packaged builds (where
`build-server.js` compiles `server/` into `server/dist/` for bundling).

```bash
npm run dev      # go run ./server (package-relative fallbacks)
npm run build    # go build ./server/...
```

Consumed by the app as a local install: `npm install ../../files-workbench-core`.
The client library ships as raw ESM source compiled by the app's bundler.
