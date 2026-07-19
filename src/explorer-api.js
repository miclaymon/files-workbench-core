import { API_BASE, API_TIMEOUT_MS, API_V } from './api-config.js'

async function _get(path, params = {}) {
  const qs = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null) qs.set(k, String(v))
  }
  const q = qs.toString()
  const url = `${API_BASE}${path}${q ? `?${q}` : ''}`

  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), API_TIMEOUT_MS)

  try {
    const res = await fetch(url, { signal: controller.signal })
    if (!res.ok) {
      const err = await res.json().catch(() => ({ detail: res.statusText }))
      throw new Error(err.detail ?? `HTTP ${res.status}`)
    }
    return res.json()
  } catch (err) {
    if (err.name === 'AbortError') throw new Error(`Request timed out: ${path}`)
    throw err
  } finally {
    clearTimeout(timer)
  }
}

export function explorerRoot(opts = {}) {
  return _get(`/_api/${API_V}/Explorer/root`, opts)
}

export function explorerHome(opts = {}) {
  return _get(`/_api/${API_V}/Explorer/home`, opts)
}

export function explorerDrives(opts = {}) {
  return _get(`/_api/${API_V}/Explorer/drives`, opts)
}

export function explorerList(opts = {}) {
  const { root, ...rest } = opts
  return _get(`/_api/${API_V}/Explorer`, { root, ...rest })
}
