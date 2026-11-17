const RUNTIME_ONLY_KEYS = new Set([
  'runtimeApplied',
  'runtimeState',
  'runtimeMessage'
])

function copyRecordFields(target, source, omitKeys) {
  for (const [k, v] of Object.entries(source)) {
    if (omitKeys && omitKeys.has(k)) continue
    target[k] = v
  }
  return target
}

export function mergeModelList(currentModels = [], incomingModels = []) {
  const cur = Array.isArray(currentModels) ? currentModels : []
  const inc = Array.isArray(incomingModels) ? incomingModels : []
  const incomingById = new Map()
  for (const m of inc) {
    if (m && m.id) incomingById.set(m.id, m)
  }

  const out = []
  const seen = new Set()
  for (const existing of cur) {
    if (!existing || !existing.id) continue
    const next = incomingById.get(existing.id)
    if (!next) continue
    copyRecordFields(existing, next, RUNTIME_ONLY_KEYS)
    if (Array.isArray(next.capabilities)) {
      existing.capabilities = [...next.capabilities]
    }
    out.push(existing)
    seen.add(existing.id)
  }

  for (const next of inc) {
    if (!next || !next.id || seen.has(next.id)) continue
    const clone = { ...next }
    for (const rk of RUNTIME_ONLY_KEYS) delete clone[rk]
    if (Array.isArray(next.capabilities)) {
      clone.capabilities = [...next.capabilities]
    }
    out.push(clone)
    seen.add(next.id)
  }
  return out
}

export function mergeProviderList(currentList = [], incomingList = []) {
  const cur = Array.isArray(currentList) ? currentList : []
  const inc = Array.isArray(incomingList) ? incomingList : []
  const incomingById = new Map()
  for (const p of inc) {
    if (p && p.id) incomingById.set(p.id, p)
  }

  const out = []
  const seen = new Set()
  const omitKeys = new Set([...RUNTIME_ONLY_KEYS, 'models'])

  for (const existing of cur) {
    if (!existing || !existing.id) continue
    const next = incomingById.get(existing.id)
    if (!next) continue
    copyRecordFields(existing, next, omitKeys)
    existing.models = mergeModelList(existing.models, next.models)
    out.push(existing)
    seen.add(existing.id)
  }

  for (const next of inc) {
    if (!next || !next.id || seen.has(next.id)) continue
    const clone = copyRecordFields({}, next, omitKeys)
    clone.models = mergeModelList([], next.models)
    out.push(clone)
    seen.add(next.id)
  }
  return out
}

export function applySavedProvider(currentList = [], savedRecord = null) {
  const cur = Array.isArray(currentList) ? currentList : []
  if (!savedRecord || !savedRecord.id) return cur
  const target = cur.find(p => p && p.id === savedRecord.id)
  if (!target) return cur
  const omitKeys = new Set([...RUNTIME_ONLY_KEYS, 'models'])
  copyRecordFields(target, savedRecord, omitKeys)
  if (Array.isArray(savedRecord.models)) {
    target.models = mergeModelList(target.models, savedRecord.models)
  }
  return cur
}

export function resolveActiveProviderId(providers = [], preferredId = '') {
  const list = Array.isArray(providers) ? providers : []
  if (!list.length) return ''
  if (preferredId && list.some(p => p && p.id === preferredId)) {
    return preferredId
  }
  return list[0]?.id || ''
}

export function captureScrollState(elements = [], win = typeof window !== 'undefined' ? window : null) {
  const visited = new Set()
  const items = []
  for (const root of elements) {
    let el = root
    while (el && typeof el === 'object' && !visited.has(el)) {
      visited.add(el)
      if (typeof el.scrollTop === 'number') {
        items.push({
          el,
          top: el.scrollTop,
          left: typeof el.scrollLeft === 'number' ? el.scrollLeft : 0
        })
      }
      el = el.parentElement || null
    }
  }
  const winScroll = win && typeof win.scrollTo === 'function'
    ? { win, x: Number(win.scrollX || 0), y: Number(win.scrollY || 0) }
    : null
  return { items, winScroll }
}

export function restoreScrollState(snapshot) {
  if (!snapshot) return
  for (const item of snapshot.items || []) {
    if (!item || !item.el) continue
    item.el.scrollTop = item.top
    if (typeof item.el.scrollLeft === 'number') {
      item.el.scrollLeft = item.left
    }
  }
  if (snapshot.winScroll && typeof snapshot.winScroll.win?.scrollTo === 'function') {
    snapshot.winScroll.win.scrollTo(snapshot.winScroll.x, snapshot.winScroll.y)
  }
}

export async function withPreservedScroll(elements, fn, options = {}) {
  const nextTickFn = options.nextTickFn || (async () => {})
  const win = options.win !== undefined ? options.win : (typeof window !== 'undefined' ? window : null)
  const snap = captureScrollState(elements, win)
  try {
    return await fn()
  } finally {
    await nextTickFn()
    restoreScrollState(snap)
  }
}
