import { mdiShield, mdiHome, mdiHarddisk } from '@mdi/js'
import { explorerRoot, explorerHome, explorerDrives } from './explorer-api.js'

// ── Explorer virtual roots ────────────────────────────────────────────────────
//
// The tree's top-level entries are "virtual" roots: their display name and icon are
// fixed here, independent of the path they point at — Root → `/`, Home → the user's
// home, Drives → `/mnt` (more later). Each carries an `mdiPath` the tree renderer
// (TreeItem) uses as the node's icon, and keeps its real `path` for navigation and
// lazy child fetches used by ExplorerPanel to present the top-level tree.
const platform = (typeof window !== 'undefined' && window.electron?.platform) ?? 'linux'

const ROOTS = [
  { load: explorerRoot,   name: 'Root',   mdiPath: mdiShield,   skipOnWin: true },
  { load: explorerHome,   name: 'Home',   mdiPath: mdiHome },
  { load: explorerDrives, name: 'Drives', mdiPath: mdiHarddisk },
]

// Fetch the virtual roots with the given visibility flags. Each result carries its
// immediate children as `_preloadedItems` (so the first level shows without a fetch)
// plus the fixed name/icon. A root is included as long as it loads — Drives shows
// even with no mounted drives (an empty node), it just has no children.
export async function loadExplorerRoots({ showHidden = false, showFiles = false, excludeCategories = 'System' } = {}) {
  const opts = { showHidden, showFiles, excludeCategories, includeMetadata: false }
  const results = await Promise.allSettled(ROOTS.map(r =>
    (r.skipOnWin && platform === 'win32') ? Promise.reject(new Error('n/a')) : r.load(opts),
  ))
  const out = []
  results.forEach((res, i) => {
    const meta = ROOTS[i]
    if (res.status !== 'fulfilled' || !res.value?.root) return
    const items = res.value.items ?? []
    out.push({ ...res.value.root, name: meta.name, mdiPath: meta.mdiPath, _preloadedItems: items })
  })
  return out
}
