import { API_BASE, CONTROL_BASE, API_TIMEOUT_MS, API_V } from './api-config.js'

async function _get(path, params = {}, signal = null) {
  const base = API_BASE
  const qs = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null) qs.set(k, String(v))
  }
  const q = qs.toString()
  const url = `${base}${path}${q ? `?${q}` : ''}`

  const timeoutController = new AbortController()
  const timer = setTimeout(() => timeoutController.abort(), API_TIMEOUT_MS)
  if (signal) signal.addEventListener('abort', () => timeoutController.abort(), { once: true })

  try {
    const res = await fetch(url, { signal: timeoutController.signal })
    clearTimeout(timer)
    if (!res.ok) {
      const err = await res.json().catch(() => ({ detail: res.statusText }))
      throw new Error(err.detail ?? `HTTP ${res.status}`)
    }
    return res.json()
  } catch (err) {
    clearTimeout(timer)
    if (err.name === 'AbortError') {
      if (signal?.aborted) throw new Error('Request cancelled')
      throw new Error(`Request timed out: ${path}`)
    }
    throw err
  }
}

async function _post(path, body = {}, signal = null) {
  const url = `${CONTROL_BASE}${path}`
  const timeoutController = new AbortController()
  const timer = setTimeout(() => timeoutController.abort(), API_TIMEOUT_MS)
  const fetchSignal = signal
    ? AbortSignal.any([timeoutController.signal, signal])
    : timeoutController.signal

  try {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal: fetchSignal,
    })
    clearTimeout(timer)
    if (!res.ok) {
      const err = await res.json().catch(() => ({ detail: res.statusText }))
      throw new Error(err.detail ?? `HTTP ${res.status}`)
    }
    return res.json()
  } catch (err) {
    clearTimeout(timer)
    if (err.name === 'AbortError') {
      if (signal?.aborted) throw new Error('Request cancelled')
      throw new Error(`Request timed out: ${path}`)
    }
    throw err
  }
}

async function _put(path, body = {}, signal = null) {
  return _mutate('PUT', path, body, signal)
}

async function _patch(path, body = {}, signal = null) {
  return _mutate('PATCH', path, body, signal)
}

// Shared PUT/PATCH sender against the control server.
async function _mutate(method, path, body = {}, signal = null) {
  const url = `${CONTROL_BASE}${path}`
  const timeoutController = new AbortController()
  const timer = setTimeout(() => timeoutController.abort(), API_TIMEOUT_MS)
  const fetchSignal = signal
    ? AbortSignal.any([timeoutController.signal, signal])
    : timeoutController.signal

  try {
    const res = await fetch(url, {
      method,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal: fetchSignal,
    })
    clearTimeout(timer)
    if (!res.ok) {
      const err = await res.json().catch(() => ({ detail: res.statusText }))
      throw new Error(err.detail ?? `HTTP ${res.status}`)
    }
    return res.json()
  } catch (err) {
    clearTimeout(timer)
    if (err.name === 'AbortError') {
      if (signal?.aborted) throw new Error('Request cancelled')
      throw new Error(`Request timed out: ${path}`)
    }
    throw err
  }
}

// ── reads ─────────────────────────────────────────────────────────────────────

export function fsStat(path) {
  return _get(`/_api/${API_V}/fs/stat`, { path })
}

// One-shot dir-size read: { size, files, done }. `done` is false while the server is
// still walking the tree (the size is a running total — "at least this much").
export function fsDirSize(path, signal) {
  return _get(`/_api/${API_V}/fs/dir_size`, { path }, signal)
}

// Poll a directory's size until the walk finishes, invoking onUpdate(size, files, done)
// on each tick so the UI can count up live. Resolves when done or when `signal` aborts;
// an already-cached (done) path resolves on the first tick — instant. Network/abort
// errors quietly stop the loop.
export async function watchDirSize(path, onUpdate, { signal, intervalMs = 250 } = {}) {
  while (!signal?.aborted) {
    let res
    try {
      res = await fsDirSize(path, signal)
    } catch {
      return   // aborted or unreachable
    }
    const done = !!res?.done
    onUpdate(res?.size ?? 0, res?.files ?? 0, done)
    if (done || signal?.aborted) return
    await new Promise((resolve) => {
      const t = setTimeout(resolve, intervalMs)
      signal?.addEventListener('abort', () => { clearTimeout(t); resolve() }, { once: true })
    })
  }
}

// opts: { includeMetadata, includeDirSize, showHidden, excludeCategories, signal }
const LIST_DIR_PAGE_SIZE = 16

export async function fsListDir(path, opts = {}) {
  const { includeMetadata = true, includeDirSize = false, showHidden = false, excludeCategories = 'System', limit = null, signal } = opts
  const pageSize = limit ?? LIST_DIR_PAGE_SIZE
  const params = { path, includeMetadata, includeDirSize, showHidden, excludeCategories, limit: pageSize, offset: 0 }

  const first = await _get(`/_api/${API_V}/fs/list_dir`, params, signal)
  // An explicit `limit` requests a single page (e.g. a lightweight directory peek) —
  // return it as-is without fetching the remaining pages. `total` still reflects the
  // full count so callers can show "+N more".
  if (limit != null || first.offset + first.items.length >= first.total) return first

  // Total is known after first page — fire all remaining pages in parallel
  const offsets = []
  for (let offset = first.items.length; offset < first.total; offset += LIST_DIR_PAGE_SIZE) {
    offsets.push(offset)
  }
  const pages = await Promise.all(
    offsets.map(offset => _get(`/_api/${API_V}/fs/list_dir`, { ...params, offset }, signal))
  )
  return { items: [...first.items, ...pages.flatMap(p => p.items)], total: first.total, offset: 0 }
}

// ── writes ────────────────────────────────────────────────────────────────────

export function fsOpenWithSystem(path, opts = {}) {
  return _post(`/_api/${API_V}/fs/open_with_system`, { path }, opts.signal)
}

export function fsOpenTerminal(path, opts = {}) {
  return _post(`/_api/${API_V}/fs/open_terminal`, { path }, opts.signal)
}

export function fsCreateFile(path, opts = {}) {
  return _post(`/_api/${API_V}/fs/create_file`, { path }, opts.signal)
}

export function fsCreateDir(path, opts = {}) {
  return _post(`/_api/${API_V}/fs/create_dir`, { path }, opts.signal)
}

export function fsWriteFile(path, content, opts = {}) {
  return _post(`/_api/${API_V}/fs/write_file`, { path, content }, opts.signal)
}

export function fsRename(path, newName, opts = {}) {
  return _post(`/_api/${API_V}/fs/rename`, { path, new_name: newName }, opts.signal)
}

// paths: string[] — move all to destDir
export function fsMove(paths, destDir, opts = {}) {
  return _post(`/_api/${API_V}/fs/move`, { paths, dest_dir: destDir }, opts.signal)
}

// paths: string[] — copy all to destDir
export function fsCopy(paths, destDir, opts = {}) {
  return _post(`/_api/${API_V}/fs/copy`, { paths, dest_dir: destDir }, opts.signal)
}

// paths: string[] — permanently delete.
// Returns { deleted } on success.
// Returns { requiresElevation, elevationMethod, elevationPaths } when the server
// responds 403 requires_elevation — caller must handle and call fsDeleteElevated.
// Throws on protected-path 403 or other errors.
export async function fsDelete(paths, opts = {}) {
  const url = `${CONTROL_BASE}/_api/${API_V}/fs/delete`
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ paths }),
    signal: opts.signal,
  })
  const data = await res.json().catch(() => ({}))
  if (res.status === 403 && data.error === 'requires_elevation') {
    return { requiresElevation: true, elevationMethod: data.elevation_method, elevationPaths: data.paths }
  }
  if (!res.ok) throw new Error(data.detail ?? `HTTP ${res.status}`)
  return data
}

export function fsDeleteElevated(paths, password, opts = {}) {
  return _post(`/_api/${API_V}/fs/delete/elevated`, { paths, password }, opts.signal)
}

// paths: string[] — move to OS trash / recycle bin.
// Same elevation-detection semantics as fsDelete.
export async function fsTrash(paths, opts = {}) {
  const url = `${CONTROL_BASE}/_api/${API_V}/fs/trash`
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ paths }),
    signal: opts.signal,
  })
  const data = await res.json().catch(() => ({}))
  if (res.status === 403 && data.error === 'requires_elevation') {
    return { requiresElevation: true, elevationMethod: data.elevation_method, elevationPaths: data.paths }
  }
  if (!res.ok) throw new Error(data.detail ?? `HTTP ${res.status}`)
  return data
}

export function fsTrashElevated(paths, password, opts = {}) {
  return _post(`/_api/${API_V}/fs/trash/elevated`, { paths, password }, opts.signal)
}

// ── exe metadata (Windows PE) ─────────────────────────────────────────────────

// Returns { name, publisher, version, description } for a Windows .exe file.
export function fsExeInfo(path, opts = {}) {
  return _get(`/_api/${API_V}/media/exe_info`, { path }, opts.signal)
}

// ── archive ───────────────────────────────────────────────────────────────────

export function fsArchiveCapabilities() {
  return _get(`/_api/${API_V}/fs/archive/capabilities`)
}

// inner: "" = archive root, "docs/" = inside docs folder
export function fsArchiveList(path, inner = '', opts = {}) {
  return _get(`/_api/${API_V}/fs/archive/ls`, { path, inner }, opts.signal)
}

// format: "zip" | "tar" | "tar.gz" | "7z"
// dest: full output path including filename
export function fsCompress(paths, format, dest, opts = {}) {
  return _post(`/_api/${API_V}/fs/compress`, { paths, format, dest }, opts.signal)
}

// Returns { dest_dir } on success.
// Returns { missingTool, tool, installApt, installDnf, installPac, installBrew }
// when the server responds 422 missing_tool — caller shows install prompt.
export async function fsDecompress(path, destDir, opts = {}) {
  const url = `${CONTROL_BASE}/_api/${API_V}/fs/decompress`
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path, dest_dir: destDir }),
    signal: opts.signal,
  })
  const data = await res.json().catch(() => ({}))
  if (res.status === 422 && data.error === 'missing_tool') {
    return {
      missingTool: true,
      tool: data.tool,
      installApt: data.install_apt,
      installDnf: data.install_dnf,
      installPac: data.install_pac,
      installBrew: data.install_brew,
    }
  }
  if (!res.ok) throw new Error(data.detail ?? `HTTP ${res.status}`)
  return data
}

// ── directory customization (.directory reads / writes) ──────────────────────
// The server reads the directory from the `path` query param on every method.

// Full customization for a directory: the resolved typed summary (name/icon/comment,
// icon resolved to an absolute path for relative/~ file icons) plus the raw editable
// groups (`sections`) from its .directory file.
export function fsCustomizationGet(path, opts = {}) {
  return _get(`/_api/${API_V}/fs/customization`, { path }, opts.signal)
}

// Set the common typed [Desktop Entry] fields, losslessly (other keys/sections/comments
// are preserved). Omit a field to keep it; pass "" to remove it.
//   customization: { name?, icon?, comment? }
export function fsCustomizationPut(path, customization, opts = {}) {
  return _put(`/_api/${API_V}/fs/customization?path=${encodeURIComponent(path)}`, customization, opts.signal)
}

// Apply generic set/delete operations to arbitrary keys, losslessly. Ops without a
// section default to the app group ([X-Files-Workbench]).
//   ops: [{ op: 'set' | 'delete', section?, key, value? }]
export function fsCustomizationPatch(path, ops, opts = {}) {
  return _patch(`/_api/${API_V}/fs/customization?path=${encodeURIComponent(path)}`, { ops }, opts.signal)
}

// Pin or unpin item names within a directory (stored in .directory
// [X-Files-Workbench] Pinned). Pinned items are grouped first in listings.
//   dir: directory path · names: item basenames · pinned: true to pin, false to unpin
export function fsPin(dir, names, pinned, opts = {}) {
  return _post(`/_api/${API_V}/fs/pin`, { path: dir, names, pinned }, opts.signal)
}

// ── preferences ───────────────────────────────────────────────────────────────

export function fsPreferencesPut(prefs, opts = {}) {
  return _put(`/_api/${API_V}/preferences`, prefs, opts.signal)
}
