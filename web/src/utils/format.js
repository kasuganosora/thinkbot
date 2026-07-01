// 通用格式化工具

/**
 * 格式化 ISO 时间字符串为 YYYY-MM-DD HH:mm
 * @param {string|null|undefined} iso
 * @returns {string}
 */
export function formatTime(iso) {
  if (!iso) return '-'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return String(iso)
  const p = n => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/**
 * 格式化为日期 YYYY-MM-DD
 */
export function formatDate(iso) {
  if (!iso) return '-'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return String(iso)
  const p = n => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

/** 数字千分位 */
export function fmtNum(n) {
  if (n == null) return '-'
  return Number(n).toLocaleString('en-US')
}
