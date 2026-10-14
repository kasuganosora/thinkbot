/** Appearance palettes. Applied as CSS variables so TDesign + custom UI stay in sync. */
export const BRAND_PALETTES = {
  indigo: { base: '#5b5fef', hover: '#4d50dc', active: '#4245c7', light: 'rgba(91, 95, 239, 0.08)', focus: 'rgba(91, 95, 239, 0.15)' },
  green: { base: '#30a46c', hover: '#2b9462', active: '#218358', light: 'rgba(48, 164, 108, 0.12)', focus: 'rgba(48, 164, 108, 0.2)' },
  blue: { base: '#0071e3', hover: '#0077ed', active: '#0060c2', light: 'rgba(0, 113, 227, 0.10)', focus: 'rgba(0, 113, 227, 0.16)' },
  purple: { base: '#5e5ce6', hover: '#6e6cf0', active: '#4b49d6', light: 'rgba(94, 92, 230, 0.12)', focus: 'rgba(94, 92, 230, 0.2)' }
}

export function applyPrimaryColor(name) {
  const p = Object.hasOwn(BRAND_PALETTES, name) ? BRAND_PALETTES[name] : BRAND_PALETTES.indigo
  const style = document.documentElement.style
  style.setProperty('--bp-accent', p.base)
  style.setProperty('--bp-accent-hover', p.hover)
  style.setProperty('--bp-accent-active', p.active)
  style.setProperty('--bp-accent-soft', p.light)
  style.setProperty('--bp-accent-soft-strong', p.focus)
  style.setProperty('--td-brand-color', p.base)
  style.setProperty('--td-brand-color-hover', p.hover)
  style.setProperty('--td-brand-color-active', p.active)
  style.setProperty('--td-brand-color-light', p.light)
  style.setProperty('--td-brand-color-focus', p.focus)
  style.setProperty('--td-brand-color-light-hover', p.focus)
}

export function applySavedBrand() {
  try {
    const saved = JSON.parse(localStorage.getItem('bp_system') || '{}')
    applyPrimaryColor(saved?.primaryColor || 'indigo')
  } catch {
    applyPrimaryColor('indigo')
  }
}
