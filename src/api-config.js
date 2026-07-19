// Data server (read-only GETs). Always an absolute base — there is no dev proxy;
// the client talks to the Go server directly in dev and packaged builds alike
// (CORS is permissive on the Go side). 127.0.0.1 instead of localhost keeps API
// calls on a separate browser HTTP connection pool from media requests
// (thumbnails, previews), so thumbnail loading never blocks directory listings.
export const API_BASE = import.meta.env.VITE_API_BASE ?? 'http://127.0.0.1:8001'

// Control server handles all mutating operations (POST/PUT).
export const CONTROL_BASE = import.meta.env.VITE_CONTROL_BASE ?? 'http://localhost:8002'

export const API_TIMEOUT_MS = 30_000
export const API_V = 'v1'
export const MEDIA_BASE = `${API_BASE}/_api/${API_V}/media`
