<template>
  <div class="ps-wrap" data-testid="bot-pty-sessions">
    <div class="ps-bar">
      <div class="ps-bar-left">
        <span class="ps-title">后台交互式会话</span>
        <span class="ps-count">{{ sessions.length }}</span>
        <span v-if="error" class="ps-error-inline" role="alert">{{ error }}</span>
        <span v-else-if="!canManage" class="ps-readonly">只读：接管与关闭需要 Bot 管理权限</span>
      </div>
      <t-button size="small" variant="outline" :loading="loading" @click="load">刷新</t-button>
    </div>

    <div v-if="canManage" class="ps-create">
      <t-input v-model="newCommand" size="small" placeholder="要启动的交互式命令，例如 sh / vim / codebuddy" @keyup.enter="createSession" />
      <t-button size="small" theme="primary" :loading="creating" :disabled="!newCommand.trim()" @click="createSession">新建会话</t-button>
    </div>

    <div class="ps-layout">
      <div class="ps-list" aria-label="会话列表">
        <t-empty v-if="!sessions.length" :description="emptyText" style="margin-top:24px" />
        <button
          v-for="s in sessions"
          :key="s.sessionId"
          class="ps-item"
          :class="{ active: s.sessionId === selectedId }"
          :data-testid="`pty-session-${s.sessionId}`"
          @click="select(s)"
        >
          <div class="ps-item-top">
            <span class="ps-sid" :title="s.sessionId">{{ s.sessionId.slice(0, 12) }}</span>
            <span class="ps-state" :class="stateClass(s.state)">{{ stateLabel(s.state) }}</span>
          </div>
          <div class="ps-item-meta">
            <span>pid {{ s.pid || '—' }}</span>
            <span v-if="s.backend">{{ s.backend }}</span>
            <span>seq {{ s.seq }}</span>
          </div>
          <div class="ps-item-meta">
            <span v-if="s.health?.verdict" class="ps-health" :title="s.health.evidence">{{ s.health.verdict }}</span>
            <span v-if="s.orphaned" class="ps-orphan">已脱离回合</span>
            <span v-if="s.reason">{{ s.reason }}</span>
          </div>
          <div v-if="canManage" class="ps-item-actions">
            <t-button size="small" variant="text" @click.stop="select(s)">接管</t-button>
            <t-button size="small" variant="text" theme="danger" :loading="closingId === s.sessionId" @click.stop="closeSession(s)">关闭会话</t-button>
          </div>
        </button>
      </div>

      <div class="ps-console">
        <SessionConsole
          v-if="selectedId && canManage"
          :key="selectedId"
          :bot-id="botId"
          :session-id="selectedId"
          :orphaned="selectedOrphaned"
          @closed="onConsoleClosed"
        />
        <t-empty v-else-if="selectedId" description="接管实时终端需要 Bot 管理权限，列表信息仍可查看" style="margin-top:80px" />
        <t-empty v-else description="从左侧选一个会话即可实时查看并接管输入" style="margin-top:80px" />
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { botPtySessionApi } from '@/api/services'
import SessionConsole from '@/components/common/SessionConsole.vue'

const props = defineProps({ botId: { type: String, required: true } })

const sessions = ref([])
const selectedId = ref('')
const loading = ref(false)
const closingId = ref('')
const creating = ref(false)
const canManage = ref(false)
const newCommand = ref('')
const error = ref('')
let timer = null

const emptyText = computed(() => error.value
  ? '暂时无法读取会话列表'
  : '当前没有后台交互式会话（agent 调用 sandbox_session_spawn 后会出现在这里）')
const selectedOrphaned = computed(() =>
  !!sessions.value.find(s => s.sessionId === selectedId.value)?.orphaned)

function stateLabel(state) {
  if (state === 'running') return '运行中'
  if (state === 'exited') return '已退出'
  if (state === 'lost') return '已丢失'
  return state || '未知'
}
function stateClass(state) {
  if (state === 'running') return 'running'
  if (state === 'lost') return 'lost'
  return 'done'
}

async function load() {
  if (!props.botId) return
  loading.value = true
  try {
    const res = await botPtySessionApi.list(props.botId)
    const list = Array.isArray(res?.sessions) ? res.sessions : []
    sessions.value = list
    canManage.value = res?.canManage === true
    error.value = ''
    if (selectedId.value && !list.find(s => s.sessionId === selectedId.value)) {
      selectedId.value = ''
    }
  } catch (e) {
    error.value = e?.message || '读取失败'
    sessions.value = []
  } finally {
    loading.value = false
  }
}

function select(s) { selectedId.value = s.sessionId }

async function createSession() {
  const cmd = newCommand.value.trim()
  if (!cmd || creating.value) return
  creating.value = true
  try {
    const v = await botPtySessionApi.create(props.botId, cmd)
    newCommand.value = ''
    await load()
    if (v?.sessionId) selectedId.value = v.sessionId
  } catch (e) {
    MessagePlugin.error(`新建失败：${e?.message || '未知错误'}`)
  } finally {
    creating.value = false
  }
}

async function closeSession(s) {
  if (closingId.value) return
  closingId.value = s.sessionId
  try {
    await botPtySessionApi.remove(props.botId, s.sessionId)
    if (selectedId.value === s.sessionId) selectedId.value = ''
    await load()
  } catch (e) {
    MessagePlugin.error(`关闭失败：${e?.message || '未知错误'}`)
  } finally {
    closingId.value = ''
  }
}

function onConsoleClosed(sid) {
  if (selectedId.value === sid) selectedId.value = ''
  load()
}

onMounted(() => {
  load()
  timer = setInterval(load, 5000)
})
onBeforeUnmount(() => {
  if (timer) { clearInterval(timer); timer = null }
})
</script>

<style scoped>
.ps-wrap { width: 100%; display: flex; flex-direction: column; min-width: 0; }
.ps-bar { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 12px; }
.ps-bar-left { display: flex; align-items: center; gap: 10px; min-width: 0; }
.ps-title { font-size: 15px; font-weight: 600; color: var(--bp-label); }
.ps-count { font-size: 12px; padding: 1px 8px; border-radius: 10px; background: var(--bp-fill, rgba(127,127,127,.12)); color: var(--bp-label-secondary); }
.ps-error-inline { font-size: 12px; color: var(--bp-danger, #d54941); }
.ps-readonly { font-size: 12px; color: var(--bp-label-tertiary); }
.ps-create { display: flex; align-items: center; gap: 8px; margin-bottom: 12px; }
.ps-create :deep(.t-input) { flex: 1; min-width: 0; }
.ps-layout { display: flex; gap: 14px; align-items: flex-start; min-width: 0; }
.ps-list { width: 250px; flex-shrink: 0; display: flex; flex-direction: column; gap: 8px; max-height: 560px; overflow-y: auto; }
.ps-console { flex: 1; min-width: 0; }
.ps-item {
  display: flex; flex-direction: column; gap: 5px; width: 100%; text-align: left;
  padding: 10px 12px; border: 1px solid var(--bp-border, rgba(127,127,127,.22));
  border-radius: 8px; background: transparent; cursor: pointer; color: var(--bp-label);
}
.ps-item:hover { border-color: var(--bp-accent); }
.ps-item.active { border-color: var(--bp-accent); background: var(--bp-fill, rgba(127,127,127,.08)); }
.ps-item-top { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.ps-sid { font-family: Menlo, Monaco, monospace; font-size: 12px; }
.ps-state { font-size: 11px; padding: 1px 6px; border-radius: 4px; white-space: nowrap; }
.ps-state.running { color: var(--bp-success); background: rgba(0,168,112,.12); }
.ps-state.done { color: var(--bp-label-tertiary); background: rgba(127,127,127,.12); }
.ps-state.lost { color: var(--bp-danger, #d54941); background: rgba(213,73,65,.12); }
.ps-item-meta { display: flex; flex-wrap: wrap; gap: 8px; font-size: 11px; color: var(--bp-label-tertiary); }
.ps-health { font-family: Menlo, Monaco, monospace; }
.ps-orphan { color: var(--bp-warning, #d9821b); }
.ps-item-actions { display: flex; gap: 4px; margin-top: 2px; }
@media (max-width: 900px) {
  .ps-layout { flex-direction: column; }
  .ps-list { width: 100%; max-height: 220px; }
}
</style>
