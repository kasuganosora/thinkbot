const DEFAULT_PREFERENCES = {
  sendKey: 'enter'
}

function storageKey(userId) {
  return `bp_user_preferences_${userId || 'default'}`
}

export function loadUserPreferences(userId) {
  try {
    const saved = JSON.parse(localStorage.getItem(storageKey(userId)) || '{}')
    if (!saved.sendKey) {
      const legacy = JSON.parse(localStorage.getItem('bp_system') || '{}')
      if (legacy.sendKey === 'enter' || legacy.sendKey === 'cmd-enter') {
        saved.sendKey = legacy.sendKey
      }
    }
    return { ...DEFAULT_PREFERENCES, ...saved }
  } catch (_) {
    return { ...DEFAULT_PREFERENCES }
  }
}

export function saveUserPreferences(userId, preferences) {
  const value = { ...DEFAULT_PREFERENCES, ...preferences }
  localStorage.setItem(storageKey(userId), JSON.stringify(value))
  return value
}
