/** Appearance palettes. Applied as CSS variables so TDesign + custom UI stay in sync. */
export const BRAND_PALETTES = {
  green: { base: '#30a46c', hover: '#2b9462', active: '#218358', light: 'rgba(48, 164, 108, 0.12)' },
  blue: { base: '#0071e3', hover: '#0077ed', active: '#0060c2', light: 'rgba(0, 113, 227, 0.10)' },
  purple: { base: '#5e5ce6', hover: '#6e6cf0', active: '#4b49d6', light: 'rgba(94, 92, 230, 0.12)' }
}

export function applyPrimaryColor(name) {
  const p = BRAND_PALETTES[name] || BRAND_PALETTES.blue
  const style = document.documentElement.style
  style.setProperty('--bp-accent', p.base)
  style.setProperty('--bp-accent-hover', p.hover)
  style.setProperty('--bp-accent-active', p.active)
  style.setProperty('--bp-accent-soft', p.light)
  style.setProperty('--td-brand-color', p.base)
  style.setProperty('--td-brand-color-hover', p.hover)
  style.setProperty('--td-brand-color-active', p.active)
  style.setProperty('--td-brand-color-light', p.light)
}

export function applySavedBrand() {
  try {
    const saved = JSON.parse(localStorage.getItem('bp_system') || '{}')
    applyPrimaryColor(saved.primaryColor || 'blue')
  } catch {
    applyPrimaryColor('blue')
  }
}
