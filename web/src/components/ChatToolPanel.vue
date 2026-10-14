<template>
  <aside v-show="!layout.toolsOpen && !layout.isNarrow" class="tool-collapsed" aria-label="工作区工具" data-testid="tool-panel-collapsed">
    <button type="button" class="rail-btn" title="展开工作区" aria-label="展开工作区" @click="layout.openTools()"><t-icon name="chevron-left" /></button>
    <button v-for="item in TABS" :key="item.key" type="button" class="rail-btn" :title="item.label" :aria-label="item.label" @click="layout.openTools(item.key)">
      <t-icon :name="item.icon" />
    </button>
  </aside>

  <div v-if="layout.toolsOpen && layout.isNarrow" class="tool-backdrop" aria-hidden="true" @click="layout.closeTools()" />
  <!-- Keep visited tools mounted while hidden. Switching Bot or leaving chat disposes them. -->
  <aside
    v-show="layout.toolsOpen"
    ref="panelEl"
    class="tool-panel"
    :class="{ 'tool-drawer': layout.isNarrow }"
    :style="{ width: layout.isNarrow ? 'min(460px, 100vw)' : panelWidth + 'px' }"
    :role="layout.isNarrow ? 'dialog' : 'complementary'"
    :aria-modal="layout.isNarrow ? true : undefined"
    aria-label="Bot 工作区"
    tabindex="-1"
    data-testid="chat-tool-panel"
  >
    <div
      v-if="!layout.isNarrow"
      class="tp-resizer"
      role="separator"
      tabindex="0"
      aria-label="调整工作区宽度"
      aria-orientation="vertical"
      :aria-valuemin="MIN_W"
      :aria-valuemax="layout.maxToolWidth"
      :aria-valuenow="panelWidth"
      :aria-valuetext="panelWidth + ' 像素'"
      data-testid="tool-panel-resizer"
      @pointerdown="startResize"
      @keydown="onResizeKey"
    />
    <div class="tp-main">
      <header class="tp-header">
        <div class="tp-heading"><span>工作区</span><span class="tp-bot-name" :title="store.activeBot?.name">{{ store.activeBot?.name || '当前 Bot' }}</span></div>
        <button ref="closeButtonEl" type="button" class="rail-btn tp-close" title="收起工作区" aria-label="收起工作区" @click="layout.closeTools()"><t-icon :name="layout.isNarrow ? 'close' : 'chevron-right'" /></button>
      </header>
      <div class="tp-scope"><t-icon name="folder" /><span>当前 Bot 的所有会话共享此工作区</span></div>
      <div class="tp-tabs" role="tablist" aria-label="工作区视图" data-testid="tool-panel-tabs" @keydown="onTabKey">
        <button
          v-for="item in TABS"
          :id="'workspace-tab-' + item.key"
          :key="item.key"
          type="button"
          role="tab"
          class="tp-tab"
          :class="{ active: layout.toolTab === item.key }"
          :data-testid="`tool-tab-${item.key}`"
          :aria-controls="'workspace-pane-' + item.key"
          :aria-selected="layout.toolTab === item.key"
          :tabindex="layout.toolTab === item.key ? 0 : -1"
          @click="layout.toolTab = item.key"
        ><t-icon :name="item.icon" /><span>{{ item.label }}</span></button>
      </div>

      <div v-if="!botId" class="pane-state" role="status"><t-icon name="folder" /><strong>选择一个 Bot 后打开工作区</strong></div>
      <template v-else>
        <section id="workspace-pane-terminal" v-show="layout.toolTab === 'terminal'" role="tabpanel" aria-labelledby="workspace-tab-terminal" class="tp-body terminal-body" data-testid="tool-pane-terminal">
          <XtermConsole
            v-if="visited.terminal"
            :key="botId"
            :connect="terminalConnection.connect"
            :exec="terminalConnection.exec"
            :allow-interrupt="true"
            title="终端"
            body-height="100%"
            class="term-console"
          />
        </section>

        <section id="workspace-pane-desktop" v-show="layout.toolTab === 'desktop'" role="tabpanel" aria-labelledby="workspace-tab-desktop" class="tp-body" data-testid="tool-pane-desktop">
          <SandboxDesktop v-if="visited.desktop" :key="botId" :bot-id="botId" scope="session" />
        </section>

        <section id="workspace-pane-files" v-show="layout.toolTab === 'files'" role="tabpanel" aria-labelledby="workspace-tab-files" class="tp-body files-body" data-testid="tool-pane-files" :aria-busy="filesLoading">
          <div class="files-head">
            <nav class="crumb" aria-label="当前文件路径">
              <t-icon name="folder" />
              <template v-for="(seg, i) in crumbs" :key="i">
                <t-icon v-if="i > 0" name="chevron-right" class="crumb-sep" />
                <button type="button" class="crumb-seg" :class="{ last: i === crumbs.length - 1 }" :aria-current="i === crumbs.length - 1 ? 'location' : undefined" data-testid="crumb-seg" @click="goCrumb(i)">{{ seg }}</button>
              </template>
            </nav>
            <div class="files-ops">
              <label class="upload-btn" data-testid="file-upload-btn">
                <t-icon name="upload" /><span>上传</span>
                <input type="file" multiple class="upload-input" aria-label="上传文件" @change="onUpload" />
              </label>
              <t-button size="small" variant="outline" :disabled="filesLoading || !!filesError" @click="startMkdir"><template #icon><t-icon name="folder-add" /></template>新建文件夹</t-button>
              <t-button size="small" variant="text" shape="square" :loading="filesLoading" title="刷新文件" aria-label="刷新文件" @click="loadFiles()"><t-icon name="refresh" /></t-button>
            </div>
            <div class="files-filter">
              <div class="file-search"><t-icon name="search" /><input v-model="fileQuery" aria-label="筛选当前目录" placeholder="筛选当前目录" /><button v-if="fileQuery" type="button" aria-label="清除文件筛选" @click="fileQuery = ''"><t-icon name="close" /></button></div>
              <select v-model="fileSort" aria-label="文件排序"><option value="name">名称排序</option><option value="mtime">最近修改</option><option value="size">文件大小</option></select>
            </div>
          </div>
          <div v-if="currentUploads.length" class="upload-queue" aria-label="上传队列" aria-live="polite">
            <div class="queue-head"><span>上传记录 · {{ currentUploads.length }}</span><button v-if="currentUploads.some(item => item.status === 'success')" type="button" @click="clearCompletedUploads">清除已完成</button></div>
            <div v-for="item in currentUploads" :key="item.id" class="queue-item" :class="{ 'queue-failed': item.status === 'error' }">
              <t-icon :name="item.status === 'success' ? 'check-circle' : item.status === 'error' ? 'error-circle' : 'loading'" :class="{ spinning: item.status === 'uploading' }" />
              <div class="queue-info"><span :title="item.path + '/' + item.name">{{ item.name }}</span><small>{{ item.status === 'error' ? item.error : item.status === 'success' ? '上传完成' : item.status === 'uploading' ? '正在上传…' : '等待上传' }}</small></div>
              <button v-if="item.status === 'error'" type="button" :aria-label="'重试上传 ' + item.name" @click="retryUpload(item)">重试</button>
              <button v-if="item.status !== 'uploading'" type="button" class="queue-remove" :aria-label="'移除上传记录 ' + item.name" @click="removeUpload(item)"><t-icon name="close" /></button>
            </div>
          </div>
          <div v-if="preview" ref="previewEl" class="file-preview" role="region" :aria-label="'文件预览：' + preview.name" :aria-busy="preview.loading" tabindex="-1" data-testid="file-preview">
            <div class="preview-head"><strong :title="preview.name">{{ preview.name }}</strong><button type="button" class="rail-btn" aria-label="关闭文件预览" @click="closePreview(true)"><t-icon name="close" /></button></div>
            <div v-if="preview.loading" class="pane-state" role="status"><t-icon name="loading" class="spinning" /><span>正在读取预览…</span></div>
            <div v-else-if="preview.error" class="pane-state error-state" role="alert"><span>{{ preview.error }}</span><t-button size="small" variant="outline" @click="previewFile(preview.entry)">重试</t-button></div>
            <template v-else><img v-if="preview.kind === 'image'" :src="preview.url" :alt="preview.name" @error="preview.error = '图片无法预览，可下载后查看。'" /><pre v-else tabindex="0" aria-label="文件文本内容">{{ preview.text }}</pre><p v-if="preview.truncated" class="preview-note">仅预览前 256 KB，下载可查看完整内容。</p></template>
          </div>
          <div v-if="filesLoading" class="pane-state" role="status"><t-icon name="loading" class="spinning" /><span>正在加载文件…</span></div>
          <div v-else-if="filesError" class="pane-state error-state" role="alert"><t-icon name="error-circle" /><strong>文件加载失败</strong><span>{{ filesError }}</span><t-button size="small" variant="outline" @click="loadFiles()">重试</t-button></div>
          <div v-else class="files-table">
            <div class="files-row files-th"><span class="col-name">名称</span><span class="col-size">大小</span><span class="col-time">修改时间</span></div>
            <button v-if="!isRoot" type="button" class="files-row file-up" data-testid="file-row-up" @click="goUp"><span class="col-name"><t-icon name="rollback" class="ic-dir" />返回上一级</span></button>
            <div v-if="mkdir.editing" class="files-row mkdir-row" data-testid="file-mkdir-row">
              <span class="col-name"><t-icon name="folder" class="ic-dir" /><input ref="mkdirInputRef" v-model="mkdir.name" class="mkdir-input" data-testid="file-mkdir-input" aria-label="新文件夹名称" placeholder="新建文件夹" :disabled="mkdir.saving" @keydown.enter.prevent="confirmMkdir" @keydown.esc.prevent="cancelMkdir" @blur="confirmMkdir" /></span>
            </div>
            <div v-for="entry in visibleFiles" :key="entry.name" class="files-row" :class="{ 'is-highlighted': highlightedFile === entry.name }" :data-file-name="entry.name" data-testid="file-row">
              <span class="col-name">
                <t-icon :name="entry.type === 'dir' ? 'folder' : 'file'" :class="entry.type === 'dir' ? 'ic-dir' : 'ic-file'" />
                <button v-if="entry.type === 'dir'" type="button" class="fname folder-link" :title="entry.name" @click="enterDir(entry.name)">{{ entry.name }}</button>
                <button v-else-if="previewKind(entry.name)" type="button" class="fname folder-link" :title="'预览 ' + entry.name" @click="previewFile(entry)">{{ entry.name }}</button>
                <span v-else class="fname" :title="entry.name">{{ entry.name }}</span>
                <button v-if="entry.type !== 'dir'" type="button" class="ic-download" title="下载" :aria-label="'下载 ' + entry.name" @click="downloadFile(entry.name)"><t-icon name="download" /></button>
              </span>
              <span class="col-size">{{ entry.type === 'dir' ? '—' : fmtSize(entry.size) }}</span>
              <span class="col-time" :title="entry.mtime">{{ fmtTime(entry.mtime) }}</span>
            </div>
            <div v-if="!visibleFiles.length && !mkdir.editing" class="pane-state"><t-icon :name="fileQuery ? 'search' : 'folder'" /><strong>{{ fileQuery ? '没有找到匹配的文件' : '这个目录还是空的' }}</strong><span>{{ fileQuery ? '试试其他名称，或清除筛选查看全部文件。' : '上传文件或新建文件夹，开始整理工作内容。' }}</span></div>
            <p v-if="highlightedFile && !files.entries.some(entry => entry.name === highlightedFile)" class="location-notice" role="status">此目录中未找到“{{ highlightedFile }}”，文件可能已移动或删除。</p>
          </div>
        </section>

        <section id="workspace-pane-status" v-show="layout.toolTab === 'status'" role="tabpanel" aria-labelledby="workspace-tab-status" class="tp-body status-body" data-testid="tool-pane-status" :aria-busy="statusLoading">
          <div class="status-head"><strong>Bot 累计用量</strong><t-button size="small" variant="text" shape="square" :loading="statusLoading" title="刷新用量" aria-label="刷新用量" @click="loadStatus"><t-icon name="refresh" /></t-button></div>
          <div v-if="statusLoading" class="pane-state" role="status"><t-icon name="loading" class="spinning" /><span>正在加载用量…</span></div>
          <div v-else-if="statusError" class="pane-state error-state" role="alert"><t-icon name="error-circle" /><strong>用量加载失败</strong><span>{{ statusError }}</span><t-button size="small" variant="outline" @click="loadStatus">重试</t-button></div>
          <template v-else-if="status">
            <div class="usage-summary"><span>累计 Token 消耗</span><strong>{{ fmtBig(status.totalTokens ?? status.contextUsed) }}</strong><p>当前 Bot 的历史累计用量</p></div>
            <div class="stat-list">
              <div class="stat-row"><span class="stat-k">累计消息</span><span class="stat-v">{{ fmtBig(status.messages) }}</span></div>
              <div class="stat-row"><span class="stat-k">当前上下文占用</span><span class="stat-unknown">暂无数据</span></div>
              <div class="stat-row"><span class="stat-k">缓存命中率</span><span class="stat-v">{{ Number.isFinite(status.cacheHitRate) ? (status.cacheHitRate * 100).toFixed(1) + '%' : '暂无数据' }}</span></div>
              <div class="stat-row"><span class="stat-k">缓存读取 Token</span><span class="stat-v">{{ fmtBig(status.cacheRead) }}</span></div>
              <div v-if="status.cacheWrite > 0" class="stat-row"><span class="stat-k">缓存写入 Token</span><span class="stat-v">{{ fmtBig(status.cacheWrite) }}</span></div>
            </div>
            <t-button block variant="outline" :loading="compacting" data-testid="status-compact" @click="doCompact"><template #icon><t-icon name="fullscreen-exit" /></template>压缩 Bot 记忆</t-button>
            <div class="skills-block"><div class="skills-title">Skill 使用记录</div><p v-if="!status.skills?.length" class="skills-empty">当前 Bot 暂无 Skill 记录</p><div v-else class="skills-list"><span v-for="skill in status.skills" :key="skill" class="skill-tag">{{ skill }}</span></div></div>
          </template>
        </section>
      </template>
    </div>
  </aside>
</template>

<script setup>
import { ref, computed, onBeforeUnmount, watch, nextTick } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { sessionToolApi } from '@/api/services'
import { useBotStore } from '@/stores/bot'
import { useLayoutStore } from '@/stores/layout'
import XtermConsole from '@/components/common/XtermConsole.vue'
import SandboxDesktop from '@/components/common/SandboxDesktop.vue'

const store = useBotStore()
const layout = useLayoutStore()
const botId = computed(() => store.activeBotId)
const TABS = [
  { key: 'terminal', label: '终端', icon: 'terminal' },
  { key: 'desktop', label: '桌面', icon: 'desktop' },
  { key: 'files', label: '文件', icon: 'folder' },
  { key: 'status', label: '用量', icon: 'chart-bar' }
]
const MIN_W = 340
const panelWidth = computed(() => Math.min(layout.maxToolWidth, Math.max(MIN_W, layout.toolWidth)))
const panelEl = ref(null)
const closeButtonEl = ref(null)
const visited = ref({})
let alive = true
let previousFocus = null

function onTabKey(event) {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const index = TABS.findIndex(item => item.key === layout.toolTab)
  const next = event.key === 'Home' ? 0 : event.key === 'End' ? TABS.length - 1 : (index + (event.key === 'ArrowRight' ? 1 : -1) + TABS.length) % TABS.length
  layout.toolTab = TABS[next].key
  nextTick(() => panelEl.value?.querySelector('[role="tab"][tabindex="0"]')?.focus())
}

function onDrawerKey(event) {
  if (!layout.toolsOpen || !layout.isNarrow) return
  if (event.key === 'Escape') {
    event.preventDefault()
    layout.closeTools()
  } else if (event.key === 'Tab') {
    const focusable = Array.from(panelEl.value?.querySelectorAll('button:not([disabled]), a[href], input:not([disabled]), textarea:not([disabled]), [tabindex="0"]') || []).filter(el => el.getClientRects().length > 0)
    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    if (!first) { event.preventDefault(); panelEl.value?.focus(); return }
    if (!panelEl.value?.contains(document.activeElement) || (!event.shiftKey && document.activeElement === last)) { event.preventDefault(); first.focus() }
    else if (event.shiftKey && (document.activeElement === first || document.activeElement === panelEl.value)) { event.preventDefault(); last.focus() }
  }
}

// Drawer focus is scoped to the overlay; the desktop panel does not trap focus.
watch(() => layout.toolsOpen && layout.isNarrow, async (open) => {
  if (open) {
    previousFocus = document.activeElement
    document.addEventListener('keydown', onDrawerKey)
    await nextTick()
    if (alive && layout.toolsOpen && layout.isNarrow) closeButtonEl.value?.focus()
  } else {
    document.removeEventListener('keydown', onDrawerKey)
    const trigger = previousFocus
    previousFocus = null
    await nextTick()
    if (alive && !layout.navigationOpen && trigger?.isConnected) trigger.focus()
  }
}, { immediate: true })

let resizeState = null
function setWidth(value) { layout.toolWidth = Math.min(layout.maxToolWidth, Math.max(MIN_W, value)) }
function startResize(event) {
  if (event.button !== 0) return
  event.preventDefault()
  stopResize()
  resizeState = { x: event.clientX, width: panelWidth.value, cursor: document.body.style.cursor, userSelect: document.body.style.userSelect }
  document.body.style.cursor = 'col-resize'
  document.body.style.userSelect = 'none'
  window.addEventListener('pointermove', onResize)
  window.addEventListener('pointerup', stopResize)
  window.addEventListener('pointercancel', stopResize)
  window.addEventListener('blur', stopResize)
}
function onResize(event) { if (resizeState) setWidth(resizeState.width + resizeState.x - event.clientX) }
function stopResize() {
  if (!resizeState) return
  document.body.style.cursor = resizeState.cursor
  document.body.style.userSelect = resizeState.userSelect
  resizeState = null
  window.removeEventListener('pointermove', onResize)
  window.removeEventListener('pointerup', stopResize)
  window.removeEventListener('pointercancel', stopResize)
  window.removeEventListener('blur', stopResize)
}
function onResizeKey(event) {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const step = event.shiftKey ? 80 : 24
  setWidth(event.key === 'Home' ? MIN_W : event.key === 'End' ? layout.maxToolWidth : panelWidth.value + (event.key === 'ArrowLeft' ? step : -step))
}
watch(() => layout.toolsOpen && !layout.isNarrow, (resizable) => { if (!resizable) stopResize() })

// Bind each mounted terminal to its Bot, including commands already in flight.
const terminalConnection = computed(() => {
  const id = botId.value
  return {
    async connect() {
      const info = await sessionToolApi.terminal(id)
      return { ...info, banner: info.connected ? `Connected to ${info.host}` : '终端未连接，请重试' }
    },
    exec: (command, options) => sessionToolApi.exec(id, command, options)
  }
})

const files = ref({ path: '/', entries: [] })
const filesLoading = ref(false)
const filesError = ref('')
const fileQuery = ref('')
const fileSort = ref('name')
const highlightedFile = ref('')
const botWorkspaces = new Map()
const visibleFiles = computed(() => {
  const query = fileQuery.value.trim().toLocaleLowerCase()
  return files.value.entries.filter(entry => !query || entry.name.toLocaleLowerCase().includes(query)).slice().sort((a, b) => {
    if (a.type !== b.type) return a.type === 'dir' ? -1 : 1
    if (fileSort.value === 'mtime') {
      const difference = (Date.parse(b.mtime) || 0) - (Date.parse(a.mtime) || 0)
      if (difference) return difference
    }
    if (fileSort.value === 'size') {
      const difference = (b.size || 0) - (a.size || 0)
      if (difference) return difference
    }
    return a.name.localeCompare(b.name, undefined, { numeric: true })
  })
})
const mkdir = ref({ editing: false, name: '', saving: false })
const mkdirInputRef = ref(null)
const crumbs = computed(() => ['/', ...String(files.value.path || '/').split('/').filter(Boolean)])
const isRoot = computed(() => files.value.path === '/')
let filesRequest = 0

async function loadFiles(path = files.value.path) {
  const id = botId.value
  if (!id) return
  const request = ++filesRequest
  if (files.value.path !== path) files.value = { path, entries: [] }
  filesLoading.value = true
  filesError.value = ''
  try {
    const result = await sessionToolApi.files(id, path)
    if (alive && id === botId.value && request === filesRequest) {
      files.value = { path: result.path || path, entries: result.entries || [] }
      return true
    }
  } catch (error) {
    if (alive && id === botId.value && request === filesRequest) filesError.value = error?.message || '暂时无法读取此目录，请重试。'
  } finally {
    if (alive && id === botId.value && request === filesRequest) filesLoading.value = false
  }
}
function navigateFiles(path) {
  closePreview()
  fileQuery.value = ''
  highlightedFile.value = ''
  mkdir.value = { editing: false, name: '', saving: false }
  loadFiles(path)
}
function enterDir(name) { navigateFiles(`${files.value.path.replace(/\/$/, '')}/${name}`) }
function goUp() { const parts = files.value.path.split('/').filter(Boolean); parts.pop(); navigateFiles('/' + parts.join('/')) }
function goCrumb(index) { navigateFiles('/' + crumbs.value.slice(1, index + 1).join('/')) }
function startMkdir() {
  if (!mkdir.value.editing) mkdir.value = { editing: true, name: '', saving: false }
  nextTick(() => mkdirInputRef.value?.focus())
}
function cancelMkdir() { if (!mkdir.value.saving) mkdir.value = { editing: false, name: '', saving: false } }
async function confirmMkdir() {
  if (!mkdir.value.editing || mkdir.value.saving) return
  const name = mkdir.value.name.trim()
  if (!name) { cancelMkdir(); return }
  const id = botId.value
  const path = files.value.path
  const editing = mkdir.value
  editing.saving = true
  try {
    await sessionToolApi.mkdir(id, path, name)
    if (!alive || id !== botId.value || editing !== mkdir.value) return
    mkdir.value = { editing: false, name: '', saving: false }
    MessagePlugin.success('文件夹已创建')
    await loadFiles()
  } catch (error) {
    if (!alive || id !== botId.value || editing !== mkdir.value) return
    editing.saving = false
    MessagePlugin.error(error?.message || '创建失败')
    nextTick(() => mkdirInputRef.value?.focus())
  }
}
async function downloadFile(name) {
  const id = botId.value
  if (!id || !name) return
  const path = `${files.value.path.replace(/\/$/, '')}/${name}`
  try {
    const response = await fetch(sessionToolApi.downloadUrl(id, path), { credentials: 'include' })
    if (!response.ok) {
      const error = await response.json().catch(() => ({}))
      throw new Error(error.message || '下载失败')
    }
    const url = URL.createObjectURL(await response.blob())
    const link = document.createElement('a')
    link.href = url
    link.download = name
    document.body.appendChild(link)
    link.click()
    link.remove()
    setTimeout(() => URL.revokeObjectURL(url), 1500)
  } catch (error) { MessagePlugin.error(error?.message || '下载失败') }
}

const MAX_UPLOAD_FILE_BYTES = 20 * 1024 * 1024
const MAX_UPLOAD_TOTAL_BYTES = 60 * 1024 * 1024
const uploads = ref([])
const currentUploads = computed(() => uploads.value.filter(item => item.botId === botId.value))
let processingUploads = false
let nextUploadId = 0
function onUpload(event) {
  const id = botId.value
  const path = files.value.path
  const input = event.target
  const picked = Array.from(input.files || [])
  input.value = ''
  if (!id) return
  let total = uploads.value.reduce((sum, item) => sum + (item.file?.size || 0), 0)
  for (const file of picked) {
    if (file.size > MAX_UPLOAD_FILE_BYTES) { MessagePlugin.warning(`${file.name} 超过单文件 ${fmtSize(MAX_UPLOAD_FILE_BYTES)} 上限，已跳过`); continue }
    if (total + file.size > MAX_UPLOAD_TOTAL_BYTES) { MessagePlugin.warning(`待上传文件总量上限为 ${fmtSize(MAX_UPLOAD_TOTAL_BYTES)}，其余文件已跳过`); break }
    total += file.size
    uploads.value.push({ id: ++nextUploadId, botId: id, path, name: file.name, file, status: 'queued', error: '' })
  }
  processUploadQueue()
}
async function processUploadQueue() {
  if (processingUploads || !alive) return
  processingUploads = true
  const changedDirectories = new Set()
  try {
    let item
    while (alive && (item = uploads.value.find(entry => entry.status === 'queued'))) {
      item.status = 'uploading'
      try {
        await sessionToolApi.upload(item.botId, item.path, item.file)
        item.status = 'success'
        item.file = null
        changedDirectories.add(`${item.botId}\n${item.path}`)
      } catch (error) {
        item.status = 'error'
        item.error = error?.message || '上传失败，请重试'
      }
    }
  } finally {
    processingUploads = false
    if (alive && changedDirectories.has(`${botId.value}\n${files.value.path}`)) loadFiles()
  }
}
function retryUpload(item) { if (item.status === 'error' && item.file) { item.status = 'queued'; item.error = ''; processUploadQueue() } }
function removeUpload(item) { if (item.status !== 'uploading') uploads.value = uploads.value.filter(entry => entry.id !== item.id) }
function clearCompletedUploads() { uploads.value = uploads.value.filter(item => item.botId !== botId.value || item.status !== 'success') }

const preview = ref(null)
const previewEl = ref(null)
let previewController = null
let previewTrigger = null
const IMAGE_PREVIEW_LIMIT = 5 * 1024 * 1024
const TEXT_PREVIEW_LIMIT = 256 * 1024
function previewKind(name) {
  if (/\.(png|jpe?g|gif|webp|avif|bmp)$/i.test(name)) return 'image'
  if (/\.(txt|md|csv|json|jsonl|ya?ml|toml|log|ini|conf|xml|html?|css|scss|js|jsx|ts|tsx|py|go|rs|java|sh|sql|svg)$/i.test(name) || /^(Dockerfile|Makefile|LICENSE|README)$/i.test(name)) return 'text'
  return ''
}
function closePreview(restoreFocus = false) {
  previewController?.abort()
  previewController = null
  if (preview.value?.url) URL.revokeObjectURL(preview.value.url)
  preview.value = null
  const trigger = previewTrigger
  previewTrigger = null
  if (restoreFocus === true) nextTick(() => { if (trigger?.isConnected) trigger.focus() })
}
async function previewFile(entry) {
  const id = botId.value
  const kind = previewKind(entry.name)
  if (!id || !kind) return
  closePreview()
  previewTrigger = document.activeElement
  const controller = new AbortController()
  previewController = controller
  const state = { name: entry.name, entry, kind, loading: true, error: '', text: '', url: '', truncated: false }
  preview.value = state
  nextTick(() => previewEl.value?.focus())
  const limit = kind === 'image' ? IMAGE_PREVIEW_LIMIT : TEXT_PREVIEW_LIMIT
  const path = `${files.value.path.replace(/\/$/, '')}/${entry.name}`
  try {
    if (kind === 'image' && entry.size > limit) throw new Error('图片超过 5 MB，请下载后查看。')
    const response = await fetch(sessionToolApi.downloadUrl(id, path), { credentials: 'include', signal: controller.signal })
    if (!response.ok) throw new Error('暂时无法读取这个文件，请重试。')
    if (kind === 'image' && Number(response.headers.get('content-length')) > limit) { await response.body.cancel(); throw new Error('图片超过 5 MB，请下载后查看。') }
    const reader = response.body.getReader()
    const chunks = []
    let length = 0
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      const remaining = limit - length
      chunks.push(value.slice(0, remaining))
      length += Math.min(value.length, remaining)
      if (value.length > remaining) { state.truncated = true; await reader.cancel(); break }
    }
    if (controller.signal.aborted || !alive || botId.value !== id) return
    if (kind === 'image') {
      if (state.truncated) throw new Error('图片超过 5 MB，请下载后查看。')
      // Only passive bitmap formats are previewed as images; SVG and HTML stay escaped text.
      const mime = { png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif', webp: 'image/webp', avif: 'image/avif', bmp: 'image/bmp' }[entry.name.split('.').pop().toLowerCase()]
      state.url = URL.createObjectURL(new Blob(chunks, { type: mime }))
    } else {
      const bytes = new Uint8Array(length)
      let offset = 0
      for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.length }
      state.text = new TextDecoder().decode(bytes)
    }
  } catch (error) {
    if (!controller.signal.aborted) state.error = error?.message || '预览失败，请重试。'
  } finally {
    if (!controller.signal.aborted && alive && botId.value === id) {
      state.loading = false
      preview.value = { ...state }
    }
  }
}

const status = ref(null)
const statusLoading = ref(false)
const statusError = ref('')
const compacting = ref(false)
let statusRequest = 0
async function loadStatus() {
  const id = botId.value
  if (!id) return
  const request = ++statusRequest
  statusLoading.value = true
  statusError.value = ''
  try {
    const result = await sessionToolApi.status(id)
    if (alive && id === botId.value && request === statusRequest) status.value = result
  } catch (error) {
    if (alive && id === botId.value && request === statusRequest) statusError.value = error?.message || '暂时无法读取用量，请重试。'
  } finally {
    if (alive && id === botId.value && request === statusRequest) statusLoading.value = false
  }
}
async function doCompact() {
  const id = botId.value
  if (!id || compacting.value) return
  compacting.value = true
  try {
    await sessionToolApi.compact(id)
    if (alive && id === botId.value) MessagePlugin.success('已触发 Bot 记忆压缩，将在后台处理')
  } catch (error) {
    if (alive && id === botId.value) MessagePlugin.error(`压缩失败：${error?.message || '请重试'}`)
  } finally { if (alive && id === botId.value) compacting.value = false }
}

function fmtSize(bytes) {
  if (bytes == null) return '—'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}
function fmtTime(iso) {
  const date = new Date(iso)
  if (isNaN(date)) return '—'
  const now = new Date()
  if (date.toDateString() === now.toDateString()) return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  if (date.getFullYear() !== now.getFullYear()) return `${date.getFullYear()}/${date.getMonth() + 1}/${date.getDate()}`
  return `${date.getMonth() + 1}/${date.getDate()}`
}
function fmtBig(value) {
  if (value == null || !Number.isFinite(Number(value))) return '暂无数据'
  const number = Number(value)
  if (number >= 1e6) return `${(number / 1e6).toFixed(1)}M`
  if (number >= 1e3) return `${(number / 1e3).toFixed(1)}K`
  return String(number)
}

watch(botId, (id, previousId) => {
  if (previousId) botWorkspaces.set(previousId, { tab: layout.toolTab, path: files.value.path, query: fileQuery.value, sort: fileSort.value })
  const saved = botWorkspaces.get(id)
  closePreview()
  filesRequest++
  statusRequest++
  visited.value = {}
  files.value = { path: saved?.path || '/', entries: [] }
  fileQuery.value = saved?.query || ''
  fileSort.value = saved?.sort || 'name'
  highlightedFile.value = ''
  if (saved?.tab) layout.toolTab = saved.tab
  filesLoading.value = false
  filesError.value = ''
  mkdir.value = { editing: false, name: '', saving: false }
  status.value = null
  statusLoading.value = false
  statusError.value = ''
  compacting.value = false
}, { flush: 'sync' })
watch([botId, () => layout.toolsOpen, () => layout.toolTab], ([id, open, tab]) => {
  if (!id || !open || visited.value[tab]) return
  visited.value[tab] = true
  if (tab === 'files') loadFiles()
  if (tab === 'status') loadStatus()
}, { immediate: true })

let handledFileRequest = null
watch([() => layout.fileRequest, botId], async ([request, id]) => {
  if (!request || request.nonce === handledFileRequest || request.botId !== String(id)) return
  handledFileRequest = request.nonce
  const parts = request.path.replace(/\\/g, '/').split('/').filter(part => part && part !== '.')
  if (parts.some(part => part === '..')) { MessagePlugin.warning('文件路径无效，请从工作区目录打开。'); return }
  const name = parts.pop() || ''
  const directory = '/' + parts.join('/')
  closePreview()
  layout.toolTab = 'files'
  visited.value.files = true
  fileQuery.value = ''
  highlightedFile.value = name
  mkdir.value = { editing: false, name: '', saving: false }
  const loaded = await loadFiles(directory)
  if (!loaded || !alive || id !== botId.value || request.nonce !== layout.fileRequest?.nonce) return
  await nextTick()
  const row = Array.from(panelEl.value?.querySelectorAll('[data-file-name]') || []).find(el => el.dataset.fileName === name)
  row?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
  row?.querySelector('button')?.focus()
}, { immediate: true })

onBeforeUnmount(() => {
  alive = false
  filesRequest++
  statusRequest++
  closePreview()
  stopResize()
  document.removeEventListener('keydown', onDrawerKey)
})
</script>

<style scoped>
.tool-panel { position: relative; display: flex; height: 100%; min-width: 0; flex-shrink: 0; border-left: var(--bp-hairline); background: var(--bp-surface); }
.tp-main { flex: 1; display: flex; flex-direction: column; min-width: 0; overflow: hidden; }
.tp-header { display: flex; align-items: center; justify-content: space-between; min-height: 60px; padding: 0 16px; gap: 8px; }
.tp-heading { display: flex; align-items: baseline; gap: 10px; min-width: 0; font-size: 14px; font-weight: 650; color: var(--bp-label); }
.tp-bot-name { font-size: 12px; font-weight: 400; color: var(--bp-label-tertiary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tp-scope { display: flex; align-items: center; gap: 6px; padding: 0 16px 12px; font-size: 11px; line-height: 1.6; color: var(--bp-label-tertiary); }
.tp-resizer { position: absolute; left: -4px; top: 0; width: 8px; height: 100%; cursor: col-resize; z-index: 5; touch-action: none; }
.tp-resizer:hover, .tp-resizer:focus-visible { background: var(--bp-accent-soft); outline-offset: -2px; }
.tool-collapsed { width: 40px; min-width: 40px; height: 100%; flex-shrink: 0; box-sizing: border-box; border-left: var(--bp-hairline); background: var(--bp-surface); display: flex; flex-direction: column; align-items: center; gap: 8px; padding: 14px 0; }
.rail-btn { width: 32px; height: 32px; flex-shrink: 0; border: 0; background: transparent; border-radius: 8px; color: var(--bp-label-tertiary); cursor: pointer; font-size: 17px; display: inline-flex; align-items: center; justify-content: center; transition: background 140ms ease, color 140ms ease; }
.rail-btn:hover { background: var(--bp-accent-soft); color: var(--bp-accent); }
.tool-collapsed .rail-btn:first-child { margin-bottom: 4px; color: var(--bp-accent); }
.tp-close { width: 30px; height: 30px; }
.tp-tabs { display: flex; gap: 4px; margin: 0 12px; padding: 4px; border-radius: 10px; background: var(--bp-surface-fill); }
.tp-tab { display: flex; align-items: center; justify-content: center; gap: 5px; flex: 1; min-width: 0; border: none; background: transparent; padding: 8px 3px; font-size: 12px; font-family: inherit; line-height: 1.3; color: var(--bp-label-secondary); cursor: pointer; white-space: nowrap; border-radius: 7px; transition: background 140ms ease, color 140ms ease; }
.tp-tab.active { color: var(--bp-accent); background: var(--bp-surface); font-weight: 600; box-shadow: var(--bp-shadow-xs); }
.tp-tab:hover { color: var(--bp-accent); }
button:focus-visible, input:focus-visible, .upload-btn:focus-within, .tp-resizer:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
.tp-body { flex: 1; min-height: 0; overflow: auto; padding: 16px 12px 12px; }
.terminal-body { display: flex; flex-direction: column; }
.term-console { flex: 1; min-height: 0; }
.term-console :deep(.xc-body) { flex: 1; min-height: 0; }
.term-console :deep(.xc-host) { display: none; }
.term-console :deep(.xc-head) { gap: 8px; flex-wrap: wrap; }
.term-console :deep(.xc-left) { gap: 7px; }
.term-console :deep(.xc-title) { font-size: 12px; }
.term-console :deep(.xc-actions) { gap: 4px; }
.term-console :deep(.xc-actions .t-button) { height: 26px; font-size: 11px; padding: 0 8px; }
.term-console :deep(.xc-status) { font-size: 11px; }
.files-head { margin-bottom: 16px; }
.crumb { display: flex; align-items: center; gap: 4px; font-size: 12px; color: var(--bp-label-secondary); margin-bottom: 12px; flex-wrap: wrap; overflow-wrap: anywhere; }
.crumb-sep { flex-shrink: 0; color: var(--bp-label-quaternary); font-size: 12px; }
.crumb-seg { border: 0; background: transparent; color: inherit; font: inherit; padding: 3px 2px; cursor: pointer; border-radius: 4px; max-width: 100%; overflow-wrap: anywhere; text-align: left; }
.crumb-seg:hover { color: var(--bp-accent); }
.crumb-seg.last { font-weight: 600; color: var(--bp-label); }
.files-ops { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }
.files-filter { display: flex; align-items: center; gap: 8px; margin-top: 14px; }
.file-search { display: flex; flex: 1; min-width: 0; gap: 6px; align-items: center; padding: 7px 8px; border: var(--bp-hairline); border-radius: 7px; color: var(--bp-label-tertiary); background: var(--bp-surface); }
.file-search:focus-within { border-color: var(--bp-accent); }
.file-search input { flex: 1; min-width: 0; width: 100%; outline: none; padding: 0; border: 0; background: transparent; font: inherit; font-size: 12px; color: var(--bp-label); }
.file-search button { display: inline-flex; align-items: center; justify-content: center; padding: 0; border: 0; background: transparent; color: inherit; cursor: pointer; }
.files-filter select { max-width: 106px; padding: 6px; border: var(--bp-hairline); border-radius: 7px; color: var(--bp-label-secondary); background: var(--bp-surface); font-size: 11px; }
.files-filter select:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
.upload-queue { padding: 10px; border: var(--bp-hairline); border-radius: 10px; background: var(--bp-bg-subtle); margin-bottom: 14px; }
.queue-head { display: flex; justify-content: space-between; gap: 8px; color: var(--bp-label-tertiary); font-size: 11px; padding: 2px 0 8px; }
.queue-head button, .queue-item > button { border: 0; padding: 2px 3px; border-radius: 4px; background: transparent; color: var(--bp-accent); font-size: 11px; cursor: pointer; }
.queue-item { display: flex; align-items: center; gap: 8px; padding: 8px 0; color: var(--bp-success); }
.queue-item + .queue-item { border-top: var(--bp-hairline); }
.queue-info { display: flex; flex: 1; min-width: 0; flex-direction: column; gap: 3px; }
.queue-info > span { font-size: 12px; color: var(--bp-label-secondary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.queue-info small { color: var(--bp-label-tertiary); font-size: 11px; overflow-wrap: anywhere; }
.queue-failed, .queue-failed small { color: var(--bp-danger); }
.queue-item > .queue-remove { color: var(--bp-label-tertiary); font-size: 14px; }
.file-preview { border: var(--bp-hairline); border-radius: 10px; overflow: hidden; background: var(--bp-bg-subtle); margin-bottom: 14px; }
.preview-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 7px 10px; border-bottom: var(--bp-hairline); }
.preview-head strong { min-width: 0; font-size: 12px; font-weight: 500; color: var(--bp-label); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.file-preview pre { max-height: 340px; overflow: auto; margin: 0; padding: 14px; font-family: var(--bp-font-mono, monospace); font-size: 11px; line-height: 1.65; tab-size: 2; color: var(--bp-label-secondary); white-space: pre; }
.file-preview img { display: block; max-width: 100%; max-height: 360px; margin: 0 auto; object-fit: contain; }
.preview-note, .location-notice { margin: 0; padding: 10px; font-size: 11px; line-height: 1.6; color: var(--bp-label-tertiary); overflow-wrap: anywhere; }
.files-row.is-highlighted { background: var(--bp-accent-soft); box-shadow: inset 3px 0 0 var(--bp-accent); }
.upload-btn { position: relative; overflow: hidden; display: inline-flex; align-items: center; gap: 4px; height: 28px; padding: 0 10px; font-size: 12px; color: var(--bp-accent); background: var(--bp-accent-soft); border: 1px solid transparent; border-radius: 6px; cursor: pointer; white-space: nowrap; }
.upload-btn:hover { background: var(--bp-accent-soft-strong); }
.upload-btn.disabled { opacity: .55; cursor: wait; }
.upload-input { position: absolute; inset: 0; width: 100%; height: 100%; opacity: 0; cursor: pointer; font-size: 0; }
.files-table { font-size: 12px; }
.files-row { display: grid; width: 100%; box-sizing: border-box; grid-template-columns: minmax(0, 1fr) 58px 64px; gap: 8px; align-items: center; padding: 10px 4px; border: 0; border-bottom: var(--bp-hairline); background: transparent; font: inherit; }
.files-row:hover { background: var(--bp-surface-fill-hover); }
.files-th { color: var(--bp-label-tertiary); font-size: 11px; }
.files-th:hover { background: transparent; }
.file-up { cursor: pointer; text-align: left; }
.file-up .col-name { grid-column: 1 / -1; color: var(--bp-label-tertiary); }
.col-name { display: flex; align-items: center; gap: 7px; color: var(--bp-label); min-width: 0; }
.fname { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.folder-link { border: 0; padding: 4px 0; border-radius: 3px; background: transparent; text-align: left; color: inherit; font: inherit; cursor: pointer; }
.folder-link:hover { color: var(--bp-accent); }
.col-size, .col-time { text-align: right; color: var(--bp-label-tertiary); font-variant-numeric: tabular-nums; }
.ic-dir, .ic-file { flex: 0 0 auto; font-size: 16px; }
.ic-dir { color: var(--bp-accent); }
.ic-file { color: var(--bp-label-tertiary); }
.ic-download { flex: 0 0 auto; display: inline-flex; align-items: center; justify-content: center; width: 24px; height: 24px; padding: 0; border: none; border-radius: 5px; background: transparent; color: var(--bp-label-tertiary); font-size: 14px; cursor: pointer; }
.ic-download:hover { color: var(--bp-accent); background: var(--bp-accent-soft); }
.mkdir-row { grid-template-columns: 1fr; background: var(--bp-accent-soft); }
.mkdir-input { flex: 1; min-width: 0; border: 1px solid var(--bp-accent); border-radius: 4px; padding: 5px 8px; font: inherit; color: var(--bp-label); background: var(--bp-surface); }
.mkdir-input:disabled { color: var(--bp-label-tertiary); }
.pane-state { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 40px 16px; text-align: center; color: var(--bp-label-tertiary); font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; }
.pane-state > .t-icon { font-size: 26px; color: var(--bp-label-quaternary); }
.pane-state strong { font-size: 13px; font-weight: 500; color: var(--bp-label-secondary); }
.error-state > .t-icon { color: var(--bp-danger); }
.status-head { display: flex; justify-content: space-between; align-items: center; color: var(--bp-label-secondary); font-size: 12px; margin-bottom: 12px; }
.status-head strong { font-weight: 500; }
.usage-summary { padding: 18px; background: var(--bp-accent-soft); border: 1px solid var(--bp-accent-soft-strong); border-radius: 12px; }
.usage-summary > span { display: block; font-size: 12px; color: var(--bp-label-secondary); }
.usage-summary > strong { display: block; padding: 8px 0 4px; font-size: 30px; line-height: 1.3; letter-spacing: -.8px; font-weight: 600; color: var(--bp-accent); font-variant-numeric: tabular-nums; }
.usage-summary > p { margin: 0; font-size: 11px; color: var(--bp-label-tertiary); }
.stat-list { margin: 10px 0 20px; }
.stat-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 13px 2px; border-bottom: var(--bp-hairline); font-size: 12px; }
.stat-k { color: var(--bp-label-secondary); }
.stat-v { font-weight: 550; color: var(--bp-label); font-variant-numeric: tabular-nums; }
.stat-unknown { color: var(--bp-label-tertiary); font-size: 11px; }
.skills-block { margin-top: 24px; }
.skills-title { font-size: 12px; font-weight: 600; color: var(--bp-label-secondary); margin-bottom: 10px; }
.skills-empty { font-size: 12px; color: var(--bp-label-tertiary); }
.skills-list { display: flex; flex-wrap: wrap; gap: 8px; }
.skill-tag { max-width: 100%; overflow-wrap: anywhere; font-size: 12px; padding: 4px 10px; border-radius: 6px; background: var(--bp-surface-fill); color: var(--bp-label-secondary); }
.tool-backdrop { position: fixed; inset: 0; background: rgba(20, 27, 47, .3); backdrop-filter: blur(2px); z-index: 60; }
.tool-drawer { position: fixed; inset: 0 0 0 auto; max-width: 100vw; height: 100dvh; z-index: 61; box-shadow: -12px 0 40px rgba(20, 27, 47, .12); padding-bottom: env(safe-area-inset-bottom); box-sizing: border-box; }
.spinning { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
@media (prefers-reduced-motion: reduce) { .spinning { animation: none; } .rail-btn, .tp-tab { transition: none; } }
</style>
