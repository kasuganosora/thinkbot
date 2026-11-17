export const OMIT_SAMPLING_PARAM = -1

export function samplingAllowed(v) {
  return typeof v === 'number' && Number.isFinite(v) && (v === OMIT_SAMPLING_PARAM || v >= 0)
}

export function normalizeBotTemperature(v, fallback = 0.7) {
  if (typeof v === 'number' && Number.isFinite(v)) {
    return v
  }
  return fallback
}

export function normalizeModelSampling(v, fallback = 0) {
  if (typeof v === 'number' && Number.isFinite(v)) {
    return v
  }
  return fallback
}

export function formatSamplingBadge(prefix, v) {
  if (v === OMIT_SAMPLING_PARAM) {
    return `${prefix} omit`
  }
  if (typeof v === 'number' && Number.isFinite(v) && v > 0) {
    return `${prefix} ${v}`
  }
  return ''
}
