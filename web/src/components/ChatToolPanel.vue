<template>
  <!-- 折叠态：仅一条竖向图标栏 -->
  <div v-if="collapsed" class="tool-collapsed" data-testid="tool-panel-collapsed">
    <button class="rail-btn" title="展开工具栏" @click="expand">
      <t-icon name="chevron-left" />
    </button>
    <button
      v-for="t in TABS"
      :key="t.key"
      class="rail-btn"
      :title="t.label"
      @click="openTab(t.key)"
    >
      <t-icon :name="t.icon" />
    </button>
  </div>

  <!-- 展开态 -->
  <div
    v-else
    class="tool-panel"
    :style="{ width: width + 'px' }"
    data-testid="chat-tool-panel"
  >
    <!-- 拖拽手柄 -->
    <div
      class="tp-resizer"
      data-testid="tool-panel-resizer"
      @mousedown="startResize"
    />

    <!-- 内容主体 -->
    <div class="tp-main">
      <!-- 顶部 tab 栏 -->
      <div class="tp-tabs" data-testid="tool-panel-tabs">
        <button
          v-for="t in TABS"
          :key="t.key"
          class="tp-tab"
          :class="{ active: tab === t.key }"
          :data-testid="`tool-tab-${t.key}`"
          @click="tab = t.key"
        >
          <t-icon :name="t.icon" />
          <span>{{ t.label }}</span>
        </button>
        <button class="tp-collapse" title="收起工具栏" data-testid="tool-panel-collapse" @click="collapse">
          <t-icon name="chevron-right" />
        </button>
      </div>

      <!-- Terminal -->
      <section v-show="tab === 'terminal'" class="tp-body terminal-body" data-testid="tool-pane-terminal">
        <div class="term-head">
          <span class="term-label">Terminal</span>
          <span class="term-conn" :class="{ ok: terminal.connected }">
            {{ terminal.connected ? '已连接' : '未连接' }}
          </span>
        </div>
        <XtermConsole
          v-if="sid"
          :key="sid"
          :connect="onTermConnect"
          :exec="onTermExec"
          :show-header="false"
          body-height="100%"
          class="term-console"
        />
      </section>

      <!-- 文件管理 -->
      <section v-show="tab === 'files'" class="tp-body files-body" data-testid="tool-pane-files">
        <div class="files-head">
          <div class="crumb">
            <t-icon name="folder" />
            <template v-for="(seg, i) in crumbs" :key="i">
              <t-icon v-if="i > 0" name="chevron-right" class="crumb-sep" />
              <span
                class="crumb-seg"
                :class="{ last: i === crumbs.length - 1 }"
                data-testid="crumb-seg"
                @click="goCrumb(i)"
              >{{ seg }}</span>
            </template>
          </div>
          <div class="files-ops">
            <label class="upload-btn" data-testid="file-upload-btn">
              <t-icon name="upload" /><span>上传</span>
              <input ref="uploadInputRef" type="file" multiple class="upload-input" @change="onUpload" />
            </label>
            <t-button size="small" variant="outline" @click="startMkdir"><template #icon><t-icon name="folder-add" /></template>新建文件夹</t-button>
            <t-button size="small" variant="outline" shape="square" @click="loadFiles"><t-icon name="refresh" /></t-button>
          </div>
        </div>
        <div class="files-table">
          <div class="files-row files-th">
            <span class="col-name">名称</span>
            <span class="col-size">大小</span>
            <span class="col-time">修改时间</span>
          </div>
          <div
            v-if="!isRoot"
            class="files-row file-up"
            data-testid="file-row-up"
            @click="goUp"
          >
            <span class="col-name"><t-icon name="rollback" class="ic-dir" /> ..</span>
            <span class="col-size"></span>
            <span class="col-time"></span>
          </div>
          <div v-if="mkdir.editing" class="files-row mkdir-row" data-testid="file-mkdir-row">
            <span class="col-name">
              <t-icon name="folder" class="ic-dir" />
              <input
                ref="mkdirInputRef"
                v-model="mkdir.name"
                class="mkdir-input"
                data-testid="file-mkdir-input"
                placeholder="新建文件夹"
                :disabled="mkdir.saving"
                @keydown.enter.prevent="confirmMkdir"
                @keydown.esc.prevent="cancelMkdir"
                @blur="confirmMkdir"
              />
            </span>
            <span class="col-size"></span>
            <span class="col-time"></span>
          </div>
          <div
            v-for="e in files.entries"
            :key="e.name"
            class="files-row"
            :class="{ clickable: e.type === 'dir' }"
            data-testid="file-row"
            @click="e.type === 'dir' && enterDir(e.name)"
          >
            <span class="col-name">
              <t-icon :name="e.type === 'dir' ? 'folder' : 'file'" :class="e.type === 'dir' ? 'ic-dir' : 'ic-file'" />
              <span class="fname">{{ e.name }}</span>
              <t-icon
                v-if="e.type !== 'dir'"
                name="download"
                class="ic-download"
                title="下载"
                @click.stop="downloadFile(e.name)"
              />
            </span>
            <span class="col-size">{{ e.type === 'dir' ? '' : fmtSize(e.size) }}</span>
            <span class="col-time">{{ fmtTime(e.mtime) }}</span>
          </div>
          <div v-if="!files.entries.length && !mkdir.editing" class="files-empty">此目录为空</div>
        </div>
      </section>

      <!-- Status -->
      <section v-show="tab === 'status'" class="tp-body status-body" data-testid="tool-pane-status">
        <div class="stat-list">
          <div class="stat-row">
            <span class="stat-k">消息数</span>
            <span class="stat-v">{{ status.messages }}</span>
          </div>
          <div class="stat-row">
            <span class="stat-k">上下文使用率</span>
            <span class="stat-v">{{ fmtK(status.contextUsed) }} / {{ status.contextLimit ? fmtK(status.contextLimit) : '--' }}</span>
          </div>
          <div class="stat-row">
            <span class="stat-k">Cache 命中率</span>
            <span class="stat-v">{{ (status.cacheHitRate * 100).toFixed(1) }}%</span>
          </div>
          <div class="stat-row">
            <span class="stat-k">Cache 读取</span>
            <span class="stat-v">{{ fmtBig(status.cacheRead) }}</span>
          </div>
          <div v-if="status.cacheWrite > 0" class="stat-row">
            <span class="stat-k">Cache 写入</span>
            <span class="stat-v">{{ fmtBig(status.cacheWrite) }}</span>
          </div>
        </div>
        <button class="compact-btn" data-testid="status-compact" @click="doCompact">
          <t-icon name="fullscreen-exit" />立即压缩
        </button>
        <div class="skills-block">
          <div class="skills-title">SKILLS</div>
          <div v-if="!status.skills || !status.skills.length" class="skills-empty">此会话未使用任何 Skill</div>
          <div v-else class="skills-list">
            <span v-for="s in status.skills" :key="s" class="skill-tag">{{ s }}</span>
          </div>
        </div>
      </section>
    </div>

    <!-- 右侧竖向图标栏 -->
    <div class="tp-rail" data-testid="tool-panel-rail">
      <button
        v-for="t in TABS"
        :key="t.key"
        class="rail-btn"
        :class="{ active: tab === t.key }"
        :title="t.label"
        @click="tab = t.key"
      >
        <t-icon :name="t.icon" />
      </button>
      <button class="rail-btn rail-trash" title="清理"><t-icon name="delete" /></button>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onBeforeUnmount, watch, nextTick } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { sessionToolApi } from '@/api/services'
import { useBotStore } from '@/stores/bot'
import XtermConsole from '@/components/common/XtermConsole.vue'

const store = useBotStore()
const TABS = [
  { key: 'terminal', label: 'Terminal', icon: 'terminal' },
  { key: 'files', label: '文件管理', icon: 'folder' },
  { key: 'status', label: 'Status', icon: 'chart-bar' }
]
const tab = ref('terminal')

// 折叠 / 宽度（持久化）
// 默认展开（localStorage 无值或为 '0' 时展开；'1' 时收起）
const MIN_W = 340
const MAX_W = 720
const collapsed = ref(() => {
  const v = localStorage.getItem('bp_tool_collapsed')
  // 未设置过 → 默认展开；仅明确设为 '1' 才收起
  return v === '1'
})()
const width = ref(Number(localStorage.getItem('bp_tool_width')) || 404)

function collapse() { collapsed.value = true; localStorage.setItem('bp_tool_collapsed', '1') }
function expand() { collapsed.value = false; localStorage.setItem('bp_tool_collapsed', '0') }
function openTab(k) {
  // 已在展开态且就是当前 tab → 再点一次收起（符合直觉的切换行为）
  if (!collapsed.value && tab.value === k) {
    collapse()
  } else {
    tab.value = k
    expand()
  }
}

// 拖拽调宽
let resizing = false
let startX = 0
let startW = 0
function startResize(e) {
  resizing = true
  startX = e.clientX
  startW = width.value
  document.body.style.cursor = 'col-resize'
  document.body.style.userSelect = 'none'
  window.addEventListener('mousemove', onResize)
  window.addEventListener('mouseup', stopResize)
}
function onResize(e) {
  if (!resizing) return
  // 面板在右侧，向左拖动变宽
  const next = startW + (startX - e.clientX)
  width.value = Math.min(MAX_W, Math.max(MIN_W, next))
}
function stopResize() {
  if (!resizing) return
  resizing = false
  document.body.style.cursor = ''
  document.body.style.userSelect = ''
  localStorage.setItem('bp_tool_width', String(width.value))
  window.removeEventListener('mousemove', onResize)
  window.removeEventListener('mouseup', stopResize)
}

// ---------- 终端 ----------
// terminal.ref 仍用于顶部标签头（tabs / 连接状态）展示
const terminal = ref({ host: 'root@host', connected: false, tabs: [] })

// 交给复用的 XtermConsole：建立连接 + 执行命令，均走会话级 API
async function onTermConnect() {
  const t = await sessionToolApi.terminal(sid.value)
  terminal.value = t
  return { host: t.host, cwd: t.cwd, connected: !!t.connected, banner: `Connected to ${t.host}` }
}
function onTermExec(cmd) {
  return sessionToolApi.exec(sid.value, cmd)
}

// ---------- 文件 ----------
const files = ref({ path: '/', entries: [] })
const uploadInputRef = ref()
const mkdir = ref({ editing: false, name: '', saving: false })
const mkdirInputRef = ref()
const crumbs = computed(() => ['/', ...String(files.value.path || '').split('/').filter(Boolean)])
const isRoot = computed(() => files.value.path === '/')

async function loadFiles() {
  if (!sid.value) return
  files.value = await sessionToolApi.files(sid.value, files.value.path)
}
function enterDir(name) {
  if (mkdir.value.editing) mkdir.value = { editing: false, name: '', saving: false }
  files.value.path = `${files.value.path.replace(/\/$/, '')}/${name}`
  loadFiles()
}
function downloadFile(name) {
  if (!sid.value) return
  const full = `${files.value.path.replace(/\/$/, '')}/${name}`
  const url = sessionToolApi.downloadUrl(sid.value, full)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
}
function goUp() {
  if (mkdir.value.editing) mkdir.value = { editing: false, name: '', saving: false }
  const parts = files.value.path.split('/').filter(Boolean)
  parts.pop()
  files.value.path = '/' + parts.join('/')
  loadFiles()
}
function goCrumb(i) {
  if (mkdir.value.editing) mkdir.value = { editing: false, name: '', saving: false }
  if (i === 0) { files.value.path = '/'; }
  else {
    const parts = crumbs.value.slice(1, i + 1)
    files.value.path = '/' + parts.join('/')
  }
  loadFiles()
}
function startMkdir() {
  if (mkdir.value.editing) {
    mkdirInputRef.value?.focus()
    return
  }
  mkdir.value = { editing: true, name: '', saving: false }
  nextTick(() => mkdirInputRef.value?.focus())
}
function cancelMkdir() {
  if (mkdir.value.saving) return
  mkdir.value = { editing: false, name: '', saving: false }
}
async function confirmMkdir() {
  if (!mkdir.value.editing || mkdir.value.saving) return
  const name = mkdir.value.name.trim()
  if (!name) { cancelMkdir(); return }
  mkdir.value.saving = true
  try {
    await sessionToolApi.mkdir(sid.value, files.value.path, name)
    mkdir.value = { editing: false, name: '', saving: false }
    MessagePlugin.success('已创建')
    await loadFiles()
  } catch (e) {
    mkdir.value.saving = false
    MessagePlugin.error(e.message || '创建失败')
    nextTick(() => mkdirInputRef.value?.focus())
  }
}
function triggerUpload() { uploadInputRef.value?.click() }
async function onUpload(e) {
  const list = Array.from(e.target.files || [])
  const failed = []
  for (const f of list) {
    try {
      await sessionToolApi.upload(sid.value, files.value.path, f)
    } catch (err) {
      failed.push({ name: f.name, message: err?.message || '上传失败' })
    }
  }
  if (failed.length) {
    for (const item of failed) {
      MessagePlugin.error(`${item.name} 上传失败：${item.message}`)
    }
    const okCount = list.length - failed.length
    if (okCount > 0) MessagePlugin.success(`已上传 ${okCount} 个文件`)
  } else if (list.length) {
    MessagePlugin.success(`已上传 ${list.length} 个文件`)
  }
  e.target.value = ''
  loadFiles()
}

// ---------- 状态 ----------
const status = ref({ messages: 0, contextUsed: 0, contextLimit: null, cacheHitRate: 0, cacheRead: 0, cacheWrite: 0, skills: [] })
async function doCompact() {
  try {
    await sessionToolApi.compact(sid.value)
    MessagePlugin.success('已触发上下文压缩')
  } catch (e) {
    MessagePlugin.error('压缩失败')
  }
}

// ---------- 工具函数 ----------
function fmtSize(b) {
  if (b == null) return ''
  if (b < 1024) return `${b} B`
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`
  return `${(b / 1024 / 1024).toFixed(1)} MB`
}
function fmtTime(iso) {
  const d = new Date(iso)
  if (isNaN(d)) return ''
  const pad = (n) => String(n).padStart(2, '0')
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`
  const now = new Date()
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
  const dayDiff = Math.floor((startOfToday - new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()) / 86400000)
  if (dayDiff === 0) return hm
  if (dayDiff === 1) return `昨天 ${hm}`
  if (dayDiff > 1 && dayDiff <= 6) return `${dayDiff}天前`
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`
}
function fmtK(n) {
  if (n >= 1000) return `${(n / 1000).toFixed(1)}K`
  return String(n)
}
function fmtBig(n) {
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M`
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}K`
  return String(n)
}

const sid = computed(() => store.activeBotId)

async function loadAll() {
  if (!sid.value) return
  try {
    status.value = await sessionToolApi.status(sid.value)
    files.value.path = '/'
    await loadFiles()
  } catch (e) {
    // 静默：工具栏非核心
  }
}

onMounted(loadAll)
onBeforeUnmount(() => {
  window.removeEventListener('mousemove', onResize)
  window.removeEventListener('mouseup', stopResize)
})
watch(sid, loadAll)
</script>

<style scoped>
.tool-panel {
  position: relative;
  display: flex;
  height: 100%;
  flex-shrink: 0;
  border-left: 1px solid #f0f0f0;
  background: #fff;
}
.tp-resizer {
  position: absolute;
  left: -3px;
  top: 0;
  width: 6px;
  height: 100%;
  cursor: col-resize;
  z-index: 5;
}
.tp-resizer:hover { background: rgba(0, 82, 217, 0.12); }
.tp-main {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
}

/* 折叠态 */
.tool-collapsed {
  width: 44px;
  flex-shrink: 0;
  border-left: 1px solid #e7e7e7;
  background: #fafbfc;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  padding: 12px 0;
}

/* tabs */
.tp-tabs {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 10px 14px 0;
  border-bottom: 1px solid #f0f0f0;
  flex-shrink: 0;
}
.tp-tab {
  display: flex;
  align-items: center;
  gap: 6px;
  border: none;
  background: none;
  padding: 8px 6px 12px;
  font-size: 14px;
  color: #8a8f99;
  cursor: pointer;
  border-bottom: 2px solid transparent;
  margin-bottom: -1px;
}
.tp-tab.active {
  color: #1d1d1f;
  font-weight: 600;
  border-bottom-color: #1d1d1f;
}
.tp-collapse {
  margin-left: auto;
  margin-bottom: 8px;
  border: none;
  background: none;
  color: #aab;
  cursor: pointer;
  font-size: 16px;
  border-radius: 6px;
  width: 26px; height: 26px;
}
.tp-collapse:hover { background: #f2f3f5; color: #555; }

.tp-body {
  flex: 1;
  overflow: auto;
  padding: 14px;
}

/* terminal */
.terminal-body { display: flex; flex-direction: column; }
.term-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 10px;
}
.term-label {
  font-size: 13px;
  font-weight: 600;
  color: #444;
}
.term-conn { font-size: 13px; color: #bbb; }
.term-conn.ok { color: #00b96b; }
.term-console { flex: 1; min-height: 0; display: flex; flex-direction: column; }
.term-console :deep(.xc-body) { flex: 1; min-height: 0; }

/* files */
.files-head { margin-bottom: 12px; }
.crumb {
  display: flex; align-items: center; gap: 6px;
  font-size: 14px; color: #555; margin-bottom: 10px; flex-wrap: wrap;
}
.crumb-sep { color: #ccc; font-size: 13px; }
.crumb-seg { cursor: pointer; }
.crumb-seg:hover { color: #0052d9; }
.crumb-seg.last { font-weight: 600; color: #1d1d1f; cursor: default; }
.crumb-seg.last:hover { color: #1d1d1f; }
.files-ops { display: flex; gap: 8px; }
.upload-btn {
  position: relative;
  overflow: hidden;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 28px;
  padding: 0 12px;
  font-size: 12px;
  color: #0052d9;
  background: #fff;
  border: 1px solid #0052d9;
  border-radius: 3px;
  cursor: pointer;
  user-select: none;
  white-space: nowrap;
  transition: background 0.2s, color 0.2s;
}
.upload-btn:hover { background: rgba(0, 82, 217, 0.08); }
.upload-btn:active { background: rgba(0, 82, 217, 0.16); }
.hidden-file {
  position: absolute;
  top: 0;
  left: 0;
  width: 100%;
  height: 100%;
  opacity: 0;
  cursor: pointer;
}
.upload-input {
  position: absolute;
  top: 0;
  left: 0;
  width: 100%;
  height: 100%;
  opacity: 0;
  cursor: pointer;
  font-size: 0;
}
.files-table { font-size: 13px; }
.files-row {
  display: grid;
  grid-template-columns: 1fr 80px 110px;
  align-items: center;
  padding: 9px 6px;
  border-bottom: 1px solid #f3f4f6;
  border-radius: 6px;
}
.files-row.clickable { cursor: pointer; }
.files-row.clickable:hover, .file-up:hover { background: #f5f7fa; }
.file-up { cursor: pointer; color: #888; }
.files-th { color: #99a; font-size: 12px; border-bottom: 1px solid #eceef1; border-radius: 0; }
.col-name { display: flex; align-items: center; gap: 10px; color: #1d1d1f; overflow: hidden; min-width: 0; }
.col-name .fname { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.col-size { text-align: right; color: #888; }
.col-time { text-align: right; color: #999; }
.ic-dir { flex: 0 0 auto; color: #4b8bf5; font-size: 16px; }
.ic-file { flex: 0 0 auto; color: #9aa3b0; font-size: 16px; }
.ic-download {
  flex: 0 0 auto;
  color: #b0b7c3;
  font-size: 15px;
  cursor: pointer;
  opacity: 0;
  transition: opacity 0.15s, color 0.15s;
}
.files-row:hover .ic-download { opacity: 1; }
.ic-download:hover { color: #0052d9; }
.files-empty { text-align: center; color: #aaa; padding: 28px 0; font-size: 13px; }
.mkdir-row { background: #f5f9ff; }
.mkdir-input {
  flex: 1 1 auto;
  min-width: 0;
  border: 1px solid #0052d9;
  border-radius: 4px;
  padding: 3px 8px;
  font-size: 13px;
  color: #1d1d1f;
  outline: none;
  background: #fff;
}
.mkdir-input:disabled { background: #f2f3f5; color: #999; }

/* status */
.status-body { padding: 18px; }
.stat-list { margin-bottom: 14px; }
.stat-row {
  display: flex; align-items: center; justify-content: space-between;
  padding: 12px 2px;
  border-bottom: 1px solid #f1f2f4;
  font-size: 14px;
}
.stat-k { color: #6a6f78; }
.stat-v { font-weight: 600; color: #1d1d1f; }
.compact-btn {
  width: 100%;
  display: flex; align-items: center; justify-content: center; gap: 8px;
  padding: 12px;
  border: none; border-radius: 10px;
  background: #f4f6f8; color: #1d1d1f;
  font-size: 14px; font-weight: 600; cursor: pointer;
  margin: 4px 0 20px;
}
.compact-btn:hover { background: #eceff3; }
.skills-title { font-size: 12px; font-weight: 700; color: #9aa0aa; letter-spacing: 0.5px; margin-bottom: 8px; }
.skills-empty { font-size: 14px; color: #9aa0aa; }
.skills-list { display: flex; flex-wrap: wrap; gap: 8px; }
.skill-tag { font-size: 12px; padding: 3px 10px; border-radius: 8px; background: #eef1f5; color: #556; }

/* right rail */
.tp-rail {
  width: 44px;
  flex-shrink: 0;
  border-left: 1px solid #f0f0f0;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  padding: 12px 0;
}
.rail-btn {
  width: 32px; height: 32px;
  border: none; background: none; border-radius: 8px;
  color: #99a; cursor: pointer; font-size: 17px;
  display: flex; align-items: center; justify-content: center;
}
.rail-btn:hover { background: #f2f3f5; color: #555; }
.rail-btn.active { background: #eef1f5; color: #1d1d1f; }
.rail-trash { margin-top: auto; color: #e06a6a; }
.rail-trash:hover { background: #fdeaea; }
</style>
