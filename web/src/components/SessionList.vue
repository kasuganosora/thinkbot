<template>
  <section class="session-panel" data-testid="session-panel" aria-label="会话列表面板">
    <div class="session-header">
      <div class="cur-bot">
        <span class="cur-bot-avatar" aria-hidden="true">{{ store.activeBot?.avatar || '🤖' }}</span>
        <span class="cur-bot-name" data-testid="session-current-bot" :title="store.activeBot?.name">{{ store.activeBot?.name || '未选择 Bot' }}</span>
      </div>
      <button v-if="!layout.isNarrow" class="nav-icon" type="button" title="收起会话列表" aria-label="收起会话列表" @click="layout.toggleSessions()">
        <t-icon name="chevron-left" />
      </button>
    </div>

    <div class="session-tools">
      <t-button class="create-session" theme="primary" variant="outline" block :disabled="!store.activeBotId" :loading="creating" data-testid="session-create-btn" @click="onCreate">
        <template #icon><t-icon name="add" /></template>
        新建会话
      </t-button>
      <div class="session-search">
        <t-input v-model="keyword" size="small" clearable placeholder="搜索会话" aria-label="搜索会话" data-testid="session-search-input">
          <template #prefix-icon><t-icon name="search" /></template>
        </t-input>
        <button class="nav-icon" type="button" :disabled="store.sessionsLoading || !store.activeBotId" title="刷新会话列表" aria-label="刷新会话列表" @click="store.loadSessions()">
          <t-loading v-if="store.sessionsLoading" size="14px" />
          <t-icon v-else name="refresh" />
        </button>
      </div>
    </div>

    <!-- 没有会话归属的系统任务需要独立保留停止入口。 -->
    <div v-if="store.unattributedRunning > 0" class="sess-system-running" data-testid="session-system-running">
      <span class="sess-running"><i class="running-dot" />系统任务运行中 ×{{ store.unattributedRunning }}</span>
      <t-button class="sess-stop" theme="danger" variant="text" size="small" :loading="stoppingAll" data-testid="session-system-stop" title="停止该 Bot 下所有正在运行的任务" @click="onStopAll">停止</t-button>
    </div>

    <div class="session-list" data-testid="session-list" :aria-busy="store.sessionsLoading">
      <div v-if="store.sessionsError" class="list-state error-state" role="alert" data-testid="session-error-state">
        <t-icon name="error-circle" />
        <strong>会话列表加载失败</strong>
        <span>检查网络连接后重试</span>
        <button class="retry-button" type="button" :disabled="store.sessionsLoading" @click="store.loadSessions()"><t-icon name="refresh" />重新加载</button>
      </div>
      <div v-if="store.sessionsLoading && !store.sessions.length" class="list-state" role="status">
        <t-loading size="small" /><span>正在加载会话…</span>
      </div>
      <section v-for="group in groupedSessions" :key="group.label" class="session-group" :aria-label="`${group.label}的会话`">
        <h3 class="group-label">{{ group.label }}<span>{{ group.sessions.length }}</span></h3>
        <div role="list">
          <div
            v-for="s in group.sessions"
            :key="s.id"
            class="session-item"
            :class="{ active: isActive(s), renaming: renamingId === s.id }"
            :data-testid="`session-item-${s.id}`"
            role="listitem"
          >
            <div class="sess-main">
              <input
                v-if="renamingId === s.id"
                :ref="(el) => setRenameRef(s.id, el)"
                v-model="renameDraft"
                class="sess-rename"
                :data-testid="`session-rename-input-${s.id}`"
                aria-label="会话名称"
                maxlength="40"
                :disabled="renameSaving"
                @keydown.enter.prevent="commitRename(s)"
                @keydown.esc.prevent="cancelRename"
                @blur="commitRename(s)"
              />
              <button
                v-else
                class="sess-select"
                type="button"
                :aria-current="isActive(s) ? 'true' : undefined"
                :aria-label="`${s.title || '新会话'}${runningOf(s) ? '，任务运行中' : ''}`"
                :title="s.title || '新会话'"
                @click="onSelect(s)"
                @keydown="onSessionKeydown"
              >
                <span class="sess-title" :data-testid="`session-title-${s.id}`" @dblclick.stop="startRename(s)">{{ s.title || '新会话' }}</span>
                <span class="sess-meta">
                  <span v-if="channelLabel(s)" class="sess-kind" :data-testid="`session-kind-${s.id}`">{{ channelLabel(s) }}</span>
                  <span v-if="s.messageCount > 0" class="sess-count">{{ s.messageCount }} 条消息</span>
                  <span class="sess-time">{{ formatTime(s.lastMsgAt || s.createdAt) }}</span>
                </span>
              </button>
              <div v-if="runningOf(s)" class="sess-run-row">
                <span class="sess-running" :data-testid="`session-running-${s.id}`"><i class="running-dot" />运行中 {{ elapsedText(s) }}</span>
                <button class="inline-stop" type="button" :disabled="stopping.has(String(s.id))" :data-testid="`session-stop-${s.id}`" :aria-label="`停止${s.title || '新会话'}的任务`" :title="`停止该会话正在运行的 ${runningOf(s).count} 个任务`" @click="onStop(s)">
                  <t-icon name="stop-circle" />{{ stopping.has(String(s.id)) ? '停止中' : '停止' }}
                </button>
              </div>
            </div>
            <t-dropdown v-if="renamingId !== s.id" :options="sessionMenu(s)" trigger="click" placement="bottom-right">
              <button class="nav-icon sess-more" type="button" :data-testid="`session-menu-${s.id}`" :aria-label="`${s.title || '新会话'}的更多操作`" aria-haspopup="menu" title="更多操作"><t-icon name="more" /></button>
            </t-dropdown>
          </div>
        </div>
      </section>
      <div v-if="!store.sessionsLoading && !store.sessionsError && !filteredSessions.length" class="list-state" data-testid="session-empty-state">
        <t-icon :name="keyword.trim() ? 'search' : 'chat'" />
        <strong>{{ keyword.trim() ? '没有匹配的会话' : store.activeBotId ? '从新会话开始' : '先选择一个 Bot' }}</strong>
        <span>{{ keyword.trim() ? '试试其他关键词' : store.activeBotId ? '提出问题，或交给 Bot 一个任务' : '选择或创建 Bot 后即可开始对话' }}</span>
        <button v-if="keyword.trim()" class="retry-button" type="button" @click="keyword = ''">清除搜索</button>
      </div>
    </div>
  </section>
</template>

<script setup>
import { computed, nextTick, ref, watch } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { useRouter } from 'vue-router'
import { useBotStore } from '@/stores/bot'
import { useLayoutStore } from '@/stores/layout'

const store = useBotStore()
const router = useRouter()
const layout = useLayoutStore()
const keyword = ref('')
const creating = ref(false)
const stopping = ref(new Set())
const stoppingAll = ref(false)
const renamingId = ref(null)
const renameDraft = ref('')
const renameInputs = new Map()
const renameSaving = ref(false)

const filteredSessions = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  return !k ? store.sessions : store.sessions.filter(s => String(s.title || '新会话').toLowerCase().includes(k))
})
const groupedSessions = computed(() => {
  const today = new Date()
  today.setHours(0, 0, 0, 0)
  const yesterday = new Date(today)
  yesterday.setDate(yesterday.getDate() - 1)
  const groups = [ { label: '今天', sessions: [] }, { label: '昨天', sessions: [] }, { label: '更早', sessions: [] } ]
  for (const session of filteredSessions.value) {
    const timestamp = new Date(session.lastMsgAt || session.createdAt).getTime()
    const index = timestamp >= today.getTime() ? 0 : timestamp >= yesterday.getTime() ? 1 : 2
    groups[index].sessions.push(session)
  }
  return groups.filter(group => group.sessions.length)
})

watch(() => store.activeBotId, () => {
  keyword.value = ''
  cancelRename()
})

function isActive(session) { return String(session.id) === String(store.activeSessionId) }
function setRenameRef(id, el) {
  if (el) renameInputs.set(id, el)
  else renameInputs.delete(id)
}
function onSelect(s) {
  store.selectSession(s.id)
  router.push({ name: 'chat-bot', params: { botId: store.activeBotId, sessionId: s.id } })
  if (layout.isNarrow) layout.closeNavigation()
}
function onSessionKeydown(event) {
  if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const buttons = [...event.currentTarget.closest('.session-list').querySelectorAll('.sess-select')]
  const current = buttons.indexOf(event.currentTarget)
  let target = current + (event.key === 'ArrowDown' ? 1 : -1)
  if (event.key === 'Home') target = 0
  if (event.key === 'End') target = buttons.length - 1
  buttons[(target + buttons.length) % buttons.length]?.focus()
}
function sessionMenu(s) {
  return [
    { content: '重命名', value: 'rename', onClick: () => startRename(s) },
    { content: '删除会话', value: 'delete', theme: 'error', onClick: () => onDelete(s) }
  ]
}
function startRename(s) {
  renamingId.value = s.id
  renameDraft.value = s.title && s.title !== '新会话' && s.title !== '默认会话' ? s.title : ''
  nextTick(() => {
    renameInputs.get(s.id)?.focus()
    renameInputs.get(s.id)?.select?.()
  })
}
function cancelRename() {
  renamingId.value = null
  renameDraft.value = ''
}
async function commitRename(s) {
  if (renameSaving.value || renamingId.value !== s.id) return
  const next = String(renameDraft.value || '').trim()
  if (!next || next === (s.title || '新会话')) {
    cancelRename()
    return
  }
  renameSaving.value = true
  try {
    await store.renameSession(s.id, next)
    cancelRename()
  } catch (e) {
    MessagePlugin.error(typeof e === 'string' ? e : e?.message || '改名失败，请重试')
  } finally {
    renameSaving.value = false
  }
}
async function onCreate() {
  if (creating.value || !store.activeBotId) return
  creating.value = true
  try {
    const sess = await store.createSession('新会话')
    if (sess) {
      keyword.value = ''
      await router.push({ name: 'chat-bot', params: { botId: store.activeBotId, sessionId: sess.id } })
      if (layout.isNarrow) layout.closeNavigation()
    }
  } catch (e) {
    MessagePlugin.error(typeof e === 'string' ? e : e?.message || '创建失败')
  } finally {
    creating.value = false
  }
}
function onDelete(session) {
  const botId = store.activeBotId
  const dialog = DialogPlugin.confirm({
    header: '删除会话',
    body: `确定删除「${session.title || '新会话'}」？该会话下的所有消息将被清除。`,
    theme: 'warning',
    onConfirm: async () => {
      try {
        await store.deleteSession(session.id)
        if (String(botId) === String(store.activeBotId) && router.currentRoute.value.name === 'chat-bot') {
          await router.replace({ name: 'chat-bot', params: { botId, sessionId: store.activeSessionId || undefined } })
        }
        MessagePlugin.success('已删除')
        dialog.destroy()
      } catch (e) {
        MessagePlugin.error(typeof e === 'string' ? e : e?.message || '删除失败')
      }
    }
  })
}

// 时长使用 store 的秒级心跳；后台任务继续保留就地停止入口。
function runningOf(s) { return store.runningBySession[String(s.id)] || null }
function elapsedText(s) {
  const r = runningOf(s)
  if (!r?.startedAt) return ''
  const ms = store.nowTick - new Date(r.startedAt).getTime()
  if (!Number.isFinite(ms) || ms < 0) return ''
  const total = Math.floor(ms / 1000)
  const m = Math.floor(total / 60)
  const sec = total % 60
  const pad = n => String(n).padStart(2, '0')
  if (m >= 60) return `${Math.floor(m / 60)}小时${pad(m % 60)}分`
  return m > 0 ? `${m}分${pad(sec)}秒` : `${sec}秒`
}
async function onStop(s) {
  const id = String(s.id)
  if (stopping.value.has(id)) return
  stopping.value.add(id)
  try {
    await store.abortSession(s.id)
    MessagePlugin.success(`已停止「${s.title || '新会话'}」中正在运行的任务`)
  } catch (e) {
    MessagePlugin.error(typeof e === 'string' ? e : e?.message || '停止失败')
  } finally {
    stopping.value.delete(id)
  }
}
async function onStopAll() {
  if (stoppingAll.value) return
  stoppingAll.value = true
  try {
    await store.abortAllRunning()
    MessagePlugin.success('已停止该 Bot 下所有正在运行的任务')
  } catch (e) {
    MessagePlugin.error(typeof e === 'string' ? e : e?.message || '停止失败')
  } finally {
    stoppingAll.value = false
  }
}
function channelLabel(s) {
  switch (s.sessionKind) {
    case 'direct': return '私聊'
    case 'group': return '群组'
    case 'topic': return '话题'
    case 'thread': return '话题串'
    case 'timeline': return '时间线'
    case 'notify': return '通知'
    default: return ''
  }
}
function formatTime(iso) {
  if (!iso) return ''
  const d = new Date(iso)
  if (!Number.isFinite(d.getTime())) return ''
  if (d.toDateString() === new Date().toDateString()) return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
  return d.toLocaleDateString('zh-CN', { month: '2-digit', day: '2-digit' })
}
</script>

<style scoped>
.session-panel { width: var(--bp-session-width, 244px); flex-shrink: 0; min-height: 0; height: 100%; background: var(--bp-bg-subtle); border-right: var(--bp-hairline); display: flex; flex-direction: column; }
.session-header { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 0 14px 0 16px; height: 64px; flex-shrink: 0; }
.cur-bot { display: flex; align-items: center; gap: 9px; min-width: 0; }
.cur-bot-avatar { font-size: 20px; flex-shrink: 0; }
.cur-bot-name { font-size: 14px; font-weight: 650; letter-spacing: var(--bp-tracking-title); color: var(--bp-label); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.nav-icon { display: inline-flex; align-items: center; justify-content: center; width: 28px; height: 28px; flex-shrink: 0; border: 0; border-radius: var(--bp-radius-sm); padding: 0; background: transparent; color: var(--bp-label-secondary); font-size: 16px; cursor: pointer; }
.nav-icon:hover { background: var(--bp-surface-fill-hover); color: var(--bp-accent); }
.nav-icon:disabled { opacity: .5; cursor: default; }
.session-tools { padding: 0 12px 12px; }
.create-session { height: 36px; font-size: 13px; font-weight: 550; background: var(--bp-surface); }
.session-search { display: flex; gap: 6px; align-items: center; margin-top: 12px; }
.session-search > .t-input__wrap { min-width: 0; flex: 1; }
.session-list { flex: 1; min-height: 0; overflow-y: auto; padding: 0 8px 16px; }
.group-label { display: flex; align-items: center; gap: 6px; margin: 14px 8px 7px; font-size: 11px; font-weight: 550; color: var(--bp-label-secondary); }
.session-group:first-of-type .group-label { margin-top: 5px; }
.group-label span { font-size: 10px; font-weight: 400; color: var(--bp-label-tertiary); }
.session-item { position: relative; display: flex; align-items: flex-start; gap: 2px; margin-bottom: 4px; padding: 2px; border: 1px solid transparent; border-radius: var(--bp-radius-md); }
.session-item:hover { background: var(--bp-surface-fill-hover); }
.session-item.active { border-color: var(--bp-accent-soft-strong); background: var(--bp-surface); box-shadow: var(--bp-shadow-xs); }
.session-item.active::before { content: ''; position: absolute; top: 13px; bottom: 13px; left: -1px; width: 3px; border-radius: 3px; background: var(--bp-accent); }
.sess-main { flex: 1; min-width: 0; }
.sess-select { display: flex; flex-direction: column; width: 100%; gap: 6px; text-align: left; border: 0; border-radius: var(--bp-radius-sm); background: transparent; padding: 9px 5px 9px 8px; cursor: pointer; color: inherit; font: inherit; }
.sess-title { display: block; width: 100%; font-size: 13px; line-height: 1.45; font-weight: 500; letter-spacing: var(--bp-tracking-body); color: var(--bp-label); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.active .sess-title { font-weight: 650; }
.sess-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 5px 7px; color: var(--bp-label-secondary); font-size: 10px; line-height: 1.5; }
.sess-kind { padding: 0 4px; border-radius: 4px; background: var(--bp-accent-soft); color: var(--bp-accent); }
.sess-time { font-variant-numeric: tabular-nums; }
.sess-more { margin: 5px 3px 0 0; width: 24px; height: 28px; opacity: .55; }
.session-item:hover .sess-more, .session-item:focus-within .sess-more { opacity: 1; }
.sess-rename { display: block; width: calc(100% - 12px); margin: 7px 6px; padding: 5px 6px; font: inherit; font-size: 13px; color: var(--bp-label); background: var(--bp-surface); border: 1px solid var(--bp-accent); border-radius: var(--bp-radius-xs); outline: none; box-shadow: 0 0 0 3px var(--bp-accent-soft); }
.sess-run-row { display: flex; flex-wrap: wrap; gap: 3px 7px; align-items: center; justify-content: space-between; padding: 0 4px 7px 8px; }
.sess-running { display: inline-flex; align-items: center; gap: 4px; border-radius: var(--bp-radius-xs); color: var(--bp-warning); font-size: 10px; font-variant-numeric: tabular-nums; }
.running-dot { width: 5px; height: 5px; flex-shrink: 0; border-radius: 50%; background: currentColor; animation: sess-running-pulse 1.2s var(--bp-ease-out) infinite; }
@keyframes sess-running-pulse { 0%, 100% { opacity: 1; } 50% { opacity: .35; } }
.inline-stop { display: inline-flex; align-items: center; gap: 3px; border: 0; padding: 2px 3px; border-radius: 4px; background: transparent; font: inherit; font-size: 10px; color: var(--bp-danger); cursor: pointer; }
.inline-stop:hover { background: var(--bp-danger-soft); }
.inline-stop:disabled { opacity: .6; cursor: wait; }
.sess-stop { flex-shrink: 0; }
.sess-system-running { display: flex; align-items: center; justify-content: space-between; gap: 6px; margin: 0 10px 10px; padding: 6px 8px; border-radius: var(--bp-radius-sm); background: var(--bp-warning-soft); }
.list-state { display: flex; flex-direction: column; align-items: center; gap: 9px; margin-top: 14px; padding: 28px 12px; text-align: center; color: var(--bp-label-secondary); font-size: 12px; line-height: 1.6; }
.list-state > .t-icon { font-size: 25px; color: var(--bp-label-tertiary); }
.list-state strong { font-size: 13px; color: var(--bp-label); font-weight: 550; }
.error-state { margin: 6px 0 10px; padding: 18px 10px; background: var(--bp-danger-soft); border-radius: var(--bp-radius-md); }
.retry-button { display: inline-flex; align-items: center; gap: 5px; border: 0; border-radius: var(--bp-radius-xs); padding: 5px 8px; background: transparent; color: var(--bp-accent); font: inherit; cursor: pointer; }
.retry-button:hover { background: var(--bp-accent-soft); }
button:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
@media (prefers-reduced-motion: reduce) { .running-dot { animation: none; } }
@media (pointer: coarse) { .nav-icon, .sess-more { min-width: 32px; min-height: 32px; opacity: 1; } .inline-stop { padding: 6px; } }
</style>
