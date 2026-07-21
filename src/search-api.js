import { API_BASE, API_V } from './api-config.js'

// Client for the filesystem search index (docs/INDEX.md). Core proxies the
// fw-indexer service under /_api/v1/, so these are plain reads against the data
// server. When the index is unavailable (e.g. a dev checkout without the binary
// built) core answers 503 — the helpers surface that as a thrown Error / a closed
// stream, so callers can degrade to "search unavailable".

const SEARCH_BASE = `${API_BASE}/_api/${API_V}`

/**
 * @typedef {object} SearchQuery
 * @property {string} [text]   the query text
 * @property {string} [scope]  restrict to a subtree
 * @property {'substring'|'prefix'|'glob'} [match]  name/path match mode (ignored when content=true)
 * @property {boolean} [content]  search file contents (full-text) instead of name/path
 * @property {string[]} [types]   extension filter (no dot)
 * @property {number} [minSize] @property {number} [maxSize]
 * @property {boolean} [dirsOnly] @property {boolean} [filesOnly]
 * @property {'name'|'path'|'size'|'modified'|'relevance'} [sort]
 * @property {boolean} [desc]
 * @property {number} [limit] @property {number} [offset]
 */

/** Build the /search query string from a SearchQuery. */
function searchParams(q = {}) {
  const p = new URLSearchParams()
  if (q.text) p.set('q', q.text)
  if (q.scope) p.set('scope', q.scope)
  if (q.match) p.set('match', q.match)
  if (q.content) p.set('content', '1')
  if (q.sort) p.set('sort', q.sort)
  if (q.desc) p.set('desc', '1')
  if (q.dirsOnly) p.set('dirsOnly', '1')
  if (q.filesOnly) p.set('filesOnly', '1')
  if (q.types?.length) p.set('type', q.types.join(','))
  if (q.minSize) p.set('minSize', String(q.minSize))
  if (q.maxSize) p.set('maxSize', String(q.maxSize))
  if (q.limit) p.set('limit', String(q.limit))
  if (q.offset) p.set('offset', String(q.offset))
  return p
}

/**
 * Run a search. Returns a ResultPage `{ results, total, nextOffset, tookMs }`.
 * @param {SearchQuery} query
 * @param {{ signal?: AbortSignal }} [opts]
 */
export async function searchIndex(query, opts = {}) {
  const res = await fetch(`${SEARCH_BASE}/search?${searchParams(query)}`, { signal: opts.signal })
  if (res.status === 503) throw new Error('search index unavailable')
  if (!res.ok) throw new Error(`search failed: HTTP ${res.status}`)
  return res.json()
}

/** Fetch index coverage/status `{ state, fileCount, dbSizeBytes, volumes }`. */
export async function indexStatus(opts = {}) {
  const res = await fetch(`${SEARCH_BASE}/index/status`, { signal: opts.signal })
  if (res.status === 503) throw new Error('search index unavailable')
  if (!res.ok) throw new Error(`index status failed: HTTP ${res.status}`)
  return res.json()
}

/**
 * Subscribe to live index changes (Server-Sent Events). `onChange` receives each
 * `{ op: 'added'|'modified'|'removed', entry }` delta so an open result set can
 * live-update. Returns an unsubscribe function.
 * @param {(change: { op: string, entry: object }) => void} onChange
 * @param {{ onError?: (e: Event) => void }} [opts]
 */
export function subscribeIndex(onChange, opts = {}) {
  const es = new EventSource(`${SEARCH_BASE}/index/subscribe`)
  es.onmessage = (ev) => {
    try {
      onChange(JSON.parse(ev.data))
    } catch {
      /* ignore malformed frame */
    }
  }
  if (opts.onError) es.onerror = opts.onError
  return () => es.close()
}
