import { CONTROL_BASE, API_V } from './api-config.js'

const OP_TIMEOUT_MS = 60_000

// Mirror of sw.js ENDPOINTS for the direct-fetch fallback path.
const ENDPOINTS = {
  rename:           { path: 'fs/rename',          method: 'POST' },
  move:             { path: 'fs/move',             method: 'POST' },
  copy:             { path: 'fs/copy',             method: 'POST' },
  delete:           { path: 'fs/delete',           method: 'POST' },
  delete_elevated:  { path: 'fs/delete/elevated',  method: 'POST' },
  trash:            { path: 'fs/trash',            method: 'POST' },
  trash_elevated:   { path: 'fs/trash/elevated',   method: 'POST' },
  compress:         { path: 'fs/compress',         method: 'POST' },
  decompress:       { path: 'fs/decompress',       method: 'POST' },
  create_file:      { path: 'fs/create_file',      method: 'POST' },
  create_dir:       { path: 'fs/create_dir',       method: 'POST' },
  write_file:       { path: 'fs/write_file',       method: 'POST' },
  open_with_system: { path: 'fs/open_with_system', method: 'POST' },
  customization:    { path: 'fs/customization',    method: 'PUT'  },
  preferences:      { path: 'preferences',         method: 'PUT'  },
}

class SwQueue {
  constructor() {
    this._pending   = new Map()  // opId → { resolve, reject, timer }
    this._localOps  = new Map()  // opId → { kind, params } — fallback buffer
    this._ready     = false
  }

  // ── Initialisation ──────────────────────────────────────────────────────────

  async init() {
    if (!('serviceWorker' in navigator)) return
    try {
      await navigator.serviceWorker.register('/sw.js', { scope: '/' })
      navigator.serviceWorker.addEventListener('message', e => this._onMessage(e))

      // Wait until a controller is active (claimed by SW).
      if (!navigator.serviceWorker.controller) {
        await new Promise(resolve =>
          navigator.serviceWorker.addEventListener('controllerchange', resolve, { once: true })
        )
      }

      await this._sendInit()
      this._ready = true
    } catch (err) {
      console.warn('[sw-queue] init failed — using direct-fetch fallback:', err)
    }
  }

  // Sends INIT and resolves when the SW replies READY (2 s timeout).
  _sendInit() {
    return new Promise(resolve => {
      const timer = setTimeout(resolve, 2000)
      const handler = e => {
        if (e.data?.type === 'READY') {
          clearTimeout(timer)
          navigator.serviceWorker.removeEventListener('message', handler)
          resolve()
        }
      }
      navigator.serviceWorker.addEventListener('message', handler)
      navigator.serviceWorker.controller.postMessage({
        type: 'INIT', controlBase: CONTROL_BASE, apiV: API_V,
      })
    })
  }

  // ── Public API ──────────────────────────────────────────────────────────────

  /**
   * Add one operation to the SW's queue.
   * Returns the opId — pass it to execute() to get the result Promise.
   */
  enqueue(kind, params) {
    const id = crypto.randomUUID()
    this._localOps.set(id, { kind, params })
    navigator.serviceWorker.controller?.postMessage({ type: 'ENQUEUE', op: { id, kind, params } })
    return id
  }

  /**
   * Drain the SW queue and return an array of Promises (one per opId, in order).
   * Each Promise resolves with the server response or rejects with an Error.
   * If the SW is unavailable, falls back to direct fetch in the main thread.
   */
  execute(opIds) {
    const promises = opIds.map(id => new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        if (this._pending.has(id)) {
          this._pending.delete(id)
          reject(new Error('Operation timed out'))
        }
      }, OP_TIMEOUT_MS)
      this._pending.set(id, { resolve, reject, timer })
    }))

    if (this._available()) {
      navigator.serviceWorker.controller.postMessage({ type: 'EXECUTE' })
      opIds.forEach(id => this._localOps.delete(id))
    } else {
      // Direct-fetch fallback when SW is not available.
      for (const id of opIds) {
        const op = this._localOps.get(id)
        this._localOps.delete(id)
        if (op) this._executeDirect(id, op.kind, op.params)
      }
    }

    return promises
  }

  /** Reject all pending Promises and tell the SW to clear its queue. */
  clear() {
    for (const { reject, timer } of this._pending.values()) {
      clearTimeout(timer)
      reject(new Error('Cancelled'))
    }
    this._pending.clear()
    this._localOps.clear()
    navigator.serviceWorker.controller?.postMessage({ type: 'CLEAR' })
  }

  // ── Internals ───────────────────────────────────────────────────────────────

  _available() {
    return this._ready && !!navigator.serviceWorker?.controller
  }

  _onMessage(event) {
    const { type, id, result, error } = event.data ?? {}
    const entry = this._pending.get(id)
    if (!entry) return
    clearTimeout(entry.timer)
    this._pending.delete(id)
    if (type === 'OP_COMPLETE') entry.resolve(result)
    else if (type === 'OP_ERROR') entry.reject(new Error(error))
  }

  async _executeDirect(id, kind, params) {
    const entry = this._pending.get(id)
    if (!entry) return
    const def = ENDPOINTS[kind]
    if (!def) {
      clearTimeout(entry.timer)
      this._pending.delete(id)
      entry.reject(new Error(`Unknown operation: ${kind}`))
      return
    }
    const url = `${CONTROL_BASE}/_api/${API_V}/${def.path}`
    try {
      const res = await fetch(url, {
        method: def.method,
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(params ?? {}),
      })
      const data = await res.json().catch(() => ({}))
      if (!this._pending.has(id)) return  // timed out while awaiting
      clearTimeout(entry.timer)
      this._pending.delete(id)
      if (res.status === 403 && data.error === 'requires_elevation') {
        entry.resolve({ requiresElevation: true, elevationMethod: data.elevation_method, elevationPaths: data.paths })
      } else if (res.status === 422 && data.error === 'missing_tool') {
        entry.resolve({ missingTool: true, tool: data.tool, installApt: data.install_apt, installDnf: data.install_dnf, installPac: data.install_pac, installBrew: data.install_brew })
      } else if (!res.ok) {
        entry.reject(new Error(data.detail ?? `HTTP ${res.status}`))
      } else {
        entry.resolve(data)
      }
    } catch (err) {
      if (!this._pending.has(id)) return
      clearTimeout(entry.timer)
      this._pending.delete(id)
      entry.reject(err)
    }
  }
}

export const swQueue = new SwQueue()
