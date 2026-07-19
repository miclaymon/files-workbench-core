import { API_BASE, CONTROL_BASE, API_V } from './api-config.js'

// ── Server-plugin RPC ─────────────────────────────────────────────────────────
//
// The client bridge to a plugin's sandboxed WASM backend. Every plugin backend is
// reached through one generic Go endpoint — POST /_api/v1/plugins/<id>/rpc — so no
// bespoke client module is needed per plugin (this replaces scm-api.js).
//
// The Go host forwards { method, params } to the plugin's `handle` export and
// returns its { ok, result } / { ok:false, error } envelope; we unwrap it here so
// callers get the result directly (or a thrown Error), matching a normal async API.

/**
 * Call a plugin backend method.
 * @param {string} pluginId  the plugin whose backend to call
 * @param {string} method    method name the backend exposes
 * @param {any}    params    JSON-serialisable params
 * @param {{ write?: boolean, signal?: AbortSignal }} [opts]
 *        write → route via the control server (mutations); default reads via data server.
 */
export async function callPluginRpc(pluginId, method, params = {}, { write = false, signal } = {}) {
  const base = write ? CONTROL_BASE : API_BASE
  const url = `${base}/_api/${API_V}/plugins/${encodeURIComponent(pluginId)}/rpc`
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ method, params }),
    signal,
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ detail: res.statusText }))
    throw new Error(err.detail ?? `plugin ${pluginId}.${method} → HTTP ${res.status}`)
  }
  const env = await res.json()
  if (env && env.ok === false) throw new Error(env.error || `plugin ${pluginId}.${method} failed`)
  return env ? env.result : undefined
}
