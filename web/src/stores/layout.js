import { computed, ref, watch } from 'vue'
import { defineStore } from 'pinia'

function readPreference(key, fallback) {
  try { return localStorage.getItem(key) ?? fallback } catch { return fallback }
}

function savePreference(key, value) {
  try { localStorage.setItem(key, String(value)) } catch { /* Private browsing may disable storage. */ }
}

export const useLayoutStore = defineStore('layout', () => {
  const viewportWidth = ref(window.innerWidth)
  const isNarrow = computed(() => viewportWidth.value < 1200)
  const savedBotRail = readPreference('bp_bot_rail', null)
  const preferBotRail = ref(savedBotRail === null ? null : savedBotRail === '1')
  const preferSessions = ref(readPreference('bp_sessions_visible', '1') !== '0')
  const botCollapsed = computed(() => !isNarrow.value && (preferBotRail.value ?? viewportWidth.value < 1600))
  const sessionsVisible = computed(() => isNarrow.value || preferSessions.value)
  const navigationOpen = ref(false)
  const navigationSection = ref('sessions')
  const fileRequest = ref(null)
  let fileRequestID = 0
  const toolsOpen = ref(!isNarrow.value && readPreference('bp_tool_collapsed', '1') === '0')
  const savedTab = readPreference('bp_tool_tab', 'terminal')
  const toolTab = ref(['terminal', 'desktop', 'files', 'status'].includes(savedTab) ? savedTab : 'terminal')
  const savedWidth = Number(readPreference('bp_tool_width', '404'))
  const preferredToolWidth = ref(Number.isFinite(savedWidth) ? Math.max(340, Math.min(720, savedWidth)) : 404)
  const maxToolWidth = computed(() => {
    if (isNarrow.value) return viewportWidth.value
    const navigationWidth = (botCollapsed.value ? 64 : 208) + (sessionsVisible.value ? 232 : 0)
    return Math.max(340, Math.min(720, viewportWidth.value - navigationWidth - 520))
  })
  const toolWidth = computed({
    get: () => Math.min(preferredToolWidth.value, maxToolWidth.value),
    set: value => { preferredToolWidth.value = Math.max(340, Math.min(maxToolWidth.value, value)) },
  })

  function closeNavigation() { navigationOpen.value = false }
  function toggleBots() {
    if (isNarrow.value) {
      navigationSection.value = 'bots'
      navigationOpen.value = true
      toolsOpen.value = false
    } else preferBotRail.value = !botCollapsed.value
  }
  function toggleSessions() {
    if (isNarrow.value) {
      navigationSection.value = 'sessions'
      navigationOpen.value = true
      toolsOpen.value = false
    } else preferSessions.value = !preferSessions.value
  }
  function openTools(tab) {
    if (tab && ['terminal', 'desktop', 'files', 'status'].includes(tab)) toolTab.value = tab
    closeNavigation()
    toolsOpen.value = true
  }
  function closeTools() { toolsOpen.value = false }
  function openFile({ botId, path }) {
    if (!botId || typeof path !== 'string' || !path.trim() || /[\x00-\x1f]/.test(path)) return
    fileRequest.value = { botId: String(botId), path, nonce: ++fileRequestID }
    openTools('files')
  }

  watch(preferBotRail, value => savePreference('bp_bot_rail', value ? '1' : '0'))
  watch(preferSessions, value => savePreference('bp_sessions_visible', value ? '1' : '0'))
  watch(toolsOpen, value => savePreference('bp_tool_collapsed', value ? '0' : '1'))
  watch(toolTab, value => savePreference('bp_tool_tab', value))
  watch(preferredToolWidth, value => savePreference('bp_tool_width', value))
  watch(isNarrow, value => { if (!value) closeNavigation() })

  return {
    viewportWidth, isNarrow, botCollapsed, sessionsVisible, navigationOpen, navigationSection,
    toolsOpen, toolTab, toolWidth, maxToolWidth, fileRequest,
    toggleBots, toggleSessions, closeNavigation, openTools, closeTools, openFile,
  }
})
