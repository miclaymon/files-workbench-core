import { API_BASE, API_V } from './api-config.js'

let _label = ''
let _t0 = 0
let _marks = []

export function perfStart(label) {
  _label = label
  _t0 = performance.now()
  _marks = []
}

export function perfMark(name) {
  if (!_t0) return
  _marks.push({ name, ms: Math.round(performance.now() - _t0) })
}

export async function perfFlush() {
  if (!_t0) return
  const entry = { label: _label, marks: _marks, ts: new Date().toISOString() }
  _t0 = 0
  _marks = []
  try {
    await fetch(`${API_BASE}/_api/${API_V}/perf`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(entry),
    })
  } catch {}
}
