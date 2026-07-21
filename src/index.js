// @files-workbench/core — the JS client library for the Files Workbench Go
// backend (which lives in this package's server/ directory).
//
// Reads go to the data server (API_BASE, port 8001), mutations to the control
// server (CONTROL_BASE, port 8002). Base URLs come from VITE_API_BASE /
// VITE_CONTROL_BASE at build time (see src/api-config.js).

export {
  API_BASE,
  CONTROL_BASE,
  API_TIMEOUT_MS,
  API_V,
  MEDIA_BASE,
} from './api-config.js'
export {
  fsStat,
  fsDirSize,
  watchDirSize,
  fsListDir,
  fsOpenWithSystem,
  fsOpenTerminal,
  fsCreateFile,
  fsCreateDir,
  fsWriteFile,
  fsRename,
  fsMove,
  fsCopy,
  fsDelete,
  fsDeleteElevated,
  fsTrash,
  fsTrashElevated,
  fsExeInfo,
  fsArchiveCapabilities,
  fsArchiveList,
  fsCompress,
  fsDecompress,
  fsCustomizationGet,
  fsCustomizationPut,
  fsCustomizationPatch,
  fsPin,
  fsPreferencesPut,
} from './fs-api.js'
export {
  explorerRoot,
  explorerHome,
  explorerDrives,
  explorerList,
} from './explorer-api.js'
export {
  loadExplorerRoots,
} from './explorer-roots.js'
export {
  swQueue,
} from './sw-queue.js'
export {
  callPluginRpc,
} from './plugin-rpc.js'
export {
  listInstalled,
  installPluginFile,
  uninstallPlugin,
  setPluginEnabled,
  listRegistry,
  installPluginUrl,
} from './plugins-api.js'
export {
  perfStart,
  perfMark,
  perfFlush,
} from './perf-log.js'
export {
  searchIndex,
  indexStatus,
  subscribeIndex,
} from './search-api.js'
