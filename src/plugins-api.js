import { API_BASE, CONTROL_BASE, API_TIMEOUT_MS, API_V } from './api-config.js'

// ── Plugin management API ─────────────────────────────────────────────────────
//
// Client wrapper for the third-party install/uninstall/enable endpoints. Reads hit
// the data server (API_BASE); mutations hit the control server (CONTROL_BASE), like
// fs-api.js. The install upload is multipart (a .zip/.vsix package), so it doesn't go
// through the JSON senders.

const PREFIX = `/_api/${API_V}/plugins`

async function unwrap(res) {
  if (!res.ok) {
    const err = await res.json().catch(() => ({ detail: res.statusText }))
    throw new Error(err.detail ?? `HTTP ${res.status}`)
  }
  return res.json()
}

// listInstalled → the full runtime plugin manifest (first-party + third-party), each
// entry stamped with { firstParty, enabled }.
export async function listInstalled() {
  const res = await fetch(`${API_BASE}${PREFIX}/manifest`)
  const data = await unwrap(res)
  return data.plugins ?? []
}

// installPluginFile uploads a package and installs it. Returns { plugin } — the served
// descriptor of the freshly installed plugin, ready to hand to the runtime loader.
export async function installPluginFile(file, { force = false } = {}) {
  const form = new FormData()
  form.append('package', file)
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), API_TIMEOUT_MS)
  try {
    const res = await fetch(`${CONTROL_BASE}${PREFIX}/install${force ? '?force=1' : ''}`, {
      method: 'POST',
      body: form, // browser sets the multipart boundary
      signal: ctrl.signal,
    })
    return await unwrap(res)
  } finally {
    clearTimeout(timer)
  }
}

// uninstallPlugin removes a third-party plugin (the server refuses first-party).
export async function uninstallPlugin(id) {
  const res = await fetch(`${CONTROL_BASE}${PREFIX}/${encodeURIComponent(id)}`, { method: 'DELETE' })
  return unwrap(res)
}

// setPluginEnabled toggles a plugin's enabled state (disabled → not auto-loaded).
export async function setPluginEnabled(id, enabled) {
  const res = await fetch(`${CONTROL_BASE}${PREFIX}/${encodeURIComponent(id)}/enabled`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  })
  return unwrap(res)
}

// listRegistry → the configured remote registry index ({ plugins, configured }). Each
// entry is { id, name, version, description, author, icon, permissions, download, hash }.
export async function listRegistry() {
  const res = await fetch(`${API_BASE}${PREFIX}/registry`)
  return unwrap(res)
}

// installPluginUrl installs a package the server downloads from the registry and
// verifies against `hash`. Returns { plugin } like installPluginFile.
export async function installPluginUrl(url, hash, { force = false } = {}) {
  const res = await fetch(`${CONTROL_BASE}${PREFIX}/install${force ? '?force=1' : ''}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ url, hash }),
  })
  return unwrap(res)
}
