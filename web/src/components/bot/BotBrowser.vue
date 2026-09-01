<template>
  <div class="browser-cookie" data-testid="bot-browser">
    <!-- 顶部操作栏 -->
    <div class="bc-toolbar">
      <div class="bc-toolbar-left">
        <div class="bc-hint">
          <t-icon name="info-circle" />
          <span>浏览器 Cookie 即账号凭据。新增/导入后，下次浏览器会话自动注入；删除需下次会话生效。</span>
        </div>
        <div v-if="cookies.length" class="bc-stats">
          <span class="stat"><b>{{ cookies.length }}</b> 条</span>
          <span class="stat-sep">·</span>
          <span class="stat"><b>{{ domainGroups.length }}</b> 个域名</span>
          <template v-if="expiredCount">
            <span class="stat-sep">·</span>
            <span class="stat stat-warn"><b>{{ expiredCount }}</b> 条已过期</span>
          </template>
        </div>
      </div>
      <div class="bc-actions">
        <t-button theme="primary" @click="openAdd">
          <template #icon><t-icon name="add" /></template>添加
        </t-button>
        <t-button variant="outline" @click="openImport">
          <template #icon><t-icon name="upload" /></template>导入
        </t-button>
        <t-button variant="outline" :disabled="!cookies.length" @click="doExport">
          <template #icon><t-icon name="download" /></template>导出
        </t-button>
        <t-button theme="danger" variant="outline" :disabled="!cookies.length" @click="confirmClear">
          <template #icon><t-icon name="delete" /></template>清空
        </t-button>
      </div>
    </div>

    <!-- 搜索栏 -->
    <div v-if="cookies.length" class="bc-searchbar">
      <t-input
        v-model="keyword"
        placeholder="搜索域名或 Cookie 名称"
        clearable
        class="bc-search"
      >
        <template #prefix-icon><t-icon name="search" /></template>
      </t-input>
      <t-button variant="text" size="small" @click="allCollapsed ? expandAll() : collapseAll()">
        {{ allCollapsed ? '展开全部' : '折叠全部' }}
      </t-button>
    </div>

    <!-- 列表 -->
    <div v-if="loading" class="bc-loading"><t-loading /></div>
    <div v-else-if="cookies.length === 0" class="bc-empty">
      <div class="empty-icon"><t-icon name="cookie" size="28px" /></div>
      <div class="empty-title">暂无 Cookie</div>
      <div class="empty-desc">
        可从 DevTools 的 Network → Headers → Cookie 直接复制粘贴导入，<br />
        也支持 storageState JSON / cookies.txt / 扩展导出格式，或手动添加。
      </div>
      <div class="empty-ops">
        <t-button theme="primary" size="small" @click="openImport">导入 Cookie</t-button>
        <t-button variant="outline" size="small" @click="openAdd">手动添加</t-button>
      </div>
    </div>
    <div v-else-if="domainGroups.length === 0" class="bc-empty">
      <div class="empty-title">没有匹配「{{ keyword }}」的 Cookie</div>
      <div class="empty-ops"><t-button variant="outline" size="small" @click="keyword = ''">清除搜索</t-button></div>
    </div>

    <!-- 按域名分组 -->
    <div v-else class="bc-groups">
      <section v-for="g in domainGroups" :key="g.domain" class="bc-group">
        <header class="bc-group-head" @click="toggleGroup(g.domain)">
          <t-icon :name="collapsed[g.domain] ? 'chevron-right' : 'chevron-down'" class="gh-caret" />
          <span class="gh-domain">{{ g.domain }}</span>
          <span class="gh-count">{{ g.items.length }}</span>
          <t-tag v-if="g.expired" size="small" theme="warning" variant="light">{{ g.expired }} 已过期</t-tag>
          <span class="gh-spacer" />
          <t-button
            size="small"
            variant="text"
            theme="danger"
            class="gh-clear"
            @click.stop="confirmClearDomain(g.domain)"
          >清空此域</t-button>
        </header>

        <ul v-show="!collapsed[g.domain]" class="bc-list">
          <li v-for="c in g.items" :key="c.id" class="bc-item" :class="{ 'is-expired': isExpired(c.expires) }">
            <div class="bc-main">
              <div class="bc-line1">
                <span class="bc-name">{{ c.name }}</span>
                <t-tag v-if="c.httpOnly" size="small" variant="outline">HttpOnly</t-tag>
                <t-tag v-if="c.secure" size="small" variant="outline" theme="success">Secure</t-tag>
                <t-tag v-if="c.sameSite" size="small" variant="outline">{{ c.sameSite }}</t-tag>
                <t-tag v-if="isExpired(c.expires)" size="small" theme="warning" variant="light">已过期</t-tag>
              </div>
              <div class="bc-value" :class="{ revealed: c._revealed }">
                <span class="bc-value-text">{{ c._revealed && c._plain ? c._plain : c.value }}</span>
                <button class="bc-linkbtn" @click="toggleReveal(c)">{{ c._revealed ? '隐藏' : '查看' }}</button>
                <button class="bc-linkbtn" @click="copyValue(c)">复制</button>
              </div>
              <div class="bc-sub">
                <span class="bc-sub-item">path: {{ c.path }}</span>
                <span class="bc-sub-item">{{ expireText(c.expires) }}</span>
              </div>
            </div>
            <div class="bc-ops">
              <t-button size="small" variant="text" @click="openEdit(c)">编辑</t-button>
              <t-button size="small" variant="text" theme="danger" @click="confirmRemove(c)">删除</t-button>
            </div>
          </li>
        </ul>
      </section>
    </div>

    <!-- 添加 / 编辑 对话框 -->
    <t-dialog
      v-model:visible="editVisible"
      :header="editing ? '编辑 Cookie' : '添加 Cookie'"
      :width="520"
      :confirm-btn="{ content: '保存', loading: saving }"
      :on-confirm="save"
      dialogClassName="browser-edit-dialog"
    >
      <t-form :data="form" label-width="92px" class="bc-form">
        <t-form-item label="域名">
          <t-input v-model="form.domain" placeholder="如 .x.com 或 www.xiaohongshu.com" />
        </t-form-item>
        <t-form-item label="名称">
          <t-input v-model="form.name" placeholder="cookie 名" />
        </t-form-item>
        <t-form-item label="值">
          <t-textarea v-model="form.value" :autosize="{ minRows: 2, maxRows: 6 }" placeholder="cookie 值（凭据，不会展示在日志）" />
        </t-form-item>
        <t-form-item label="路径">
          <t-input v-model="form.path" placeholder="/" />
        </t-form-item>
        <t-form-item label="过期(秒)">
          <t-input v-model="form.expires" placeholder="0 = 会话级" />
        </t-form-item>
        <t-form-item label="属性">
          <t-space>
            <t-checkbox v-model="form.httpOnly">HttpOnly</t-checkbox>
            <t-checkbox v-model="form.secure">Secure</t-checkbox>
          </t-space>
        </t-form-item>
        <t-form-item label="SameSite">
          <t-select v-model="form.sameSite" :options="sameSiteOptions" />
        </t-form-item>
      </t-form>
    </t-dialog>

    <!-- 导入对话框 -->
    <t-dialog
      v-model:visible="importVisible"
      header="导入 Cookie"
      :width="620"
      :confirm-btn="{ content: '导入', loading: importing }"
      :on-confirm="confirmImport"
      dialogClassName="browser-import-dialog"
    >
      <div class="bc-imp-tip">
        <t-icon name="lightbulb" />
        <div>
          最省事的做法：打开目标网站 → F12 → <b>Network</b> → 任选一个请求 →
          <b>Headers</b> → 找到 <b>Cookie</b> 字段 → 右键 Copy value → 粘贴到下方，
          并填写「适用域名」。
        </div>
      </div>

      <div class="bc-field">
        <label class="lbl">粘贴内容</label>
        <t-textarea
          v-model="importText"
          :autosize="{ minRows: 7, maxRows: 14 }"
          placeholder="支持以下任一格式：&#10;· a=1; b=2; c=3            （DevTools 的 Cookie 请求头，最常用）&#10;· {&quot;cookies&quot;:[{...}]}        （Playwright storageState）&#10;· [{&quot;name&quot;:...,&quot;domain&quot;:...}]   （浏览器扩展导出）&#10;· # Netscape HTTP Cookie File  （cookies.txt）"
        />
        <div class="bc-detect">
          <template v-if="!importText.trim()">
            <span class="dt-muted">等待粘贴内容…</span>
          </template>
          <template v-else>
            <t-tag size="small" :theme="detected.theme" variant="light">{{ detected.label }}</t-tag>
            <span class="dt-desc">{{ detected.desc }}</span>
          </template>
        </div>
      </div>

      <!-- Cookie header 格式不含域名，必须由用户提供 -->
      <div v-if="detected.needDomain" class="bc-field">
        <label class="lbl">
          适用域名 <span class="req">必填</span>
        </label>
        <t-input v-model="importDomain" placeholder="如 www.xiaohongshu.com 或 .x.com（含子域用点号开头）" />
        <div class="bc-help">该格式只含 name=value，没有域名信息，需指明这些 Cookie 属于哪个站点。</div>
      </div>

      <t-checkbox v-model="importClear">导入前清空现有 Cookie</t-checkbox>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, computed, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botBrowserCookieApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const cookies = ref([])
const loading = ref(false)
const saving = ref(false)
const keyword = ref('')
const collapsed = reactive({})

const editVisible = ref(false)
const editing = ref(null)
const form = reactive({ domain: '', name: '', value: '', path: '/', expires: '0', httpOnly: false, secure: false, sameSite: '' })

const importVisible = ref(false)
const importText = ref('')
const importClear = ref(false)
const importDomain = ref('')
const importing = ref(false)

const sameSiteOptions = [
  { label: '无 (空)', value: '' },
  { label: 'Lax', value: 'Lax' },
  { label: 'Strict', value: 'Strict' },
  { label: 'None', value: 'None' }
]

/** 与后端 parseBrowserCookieImport 的判定保持一致，用于实时提示与是否要求填域名。 */
const detected = computed(() => {
  const raw = importText.value.trim()
  if (!raw) return { label: '', desc: '', theme: 'default', needDomain: false }
  if (raw.startsWith('# Netscape') || (raw.includes('\t') && !raw.startsWith('{') && !raw.startsWith('['))) {
    return { label: 'cookies.txt', desc: '已识别为 Netscape 格式，含完整域名与过期信息。', theme: 'primary', needDomain: false }
  }
  if (raw.startsWith('{') || raw.startsWith('[')) {
    return { label: 'JSON', desc: '已识别为 storageState / 扩展导出格式，含完整域名与过期信息。', theme: 'primary', needDomain: false }
  }
  const body = raw.replace(/^cookie:\s*/i, '')
  if (/[^\s;=]+=/.test(body)) {
    const n = body.split(';').filter(s => s.includes('=') && s.trim().indexOf('=') > 0).length
    return {
      label: 'Cookie 请求头',
      desc: `已识别为 DevTools Cookie 头，约 ${n} 项；将按会话级 Cookie 导入（path=/）。`,
      theme: 'success',
      needDomain: true
    }
  }
  return { label: '无法识别', desc: '未匹配任何已知格式，请检查粘贴内容。', theme: 'warning', needDomain: false }
})

const nowSec = () => Math.floor(Date.now() / 1000)
function isExpired(exp) {
  const e = Number(exp) || 0
  return e > 0 && e < nowSec()
}
const expiredCount = computed(() => cookies.value.filter(c => isExpired(c.expires)).length)

/** 按域名分组，并按「域名」排序；组内按 name 排序。 */
const domainGroups = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  const map = new Map()
  for (const c of cookies.value) {
    if (kw && !(`${c.domain}`.toLowerCase().includes(kw) || `${c.name}`.toLowerCase().includes(kw))) continue
    if (!map.has(c.domain)) map.set(c.domain, [])
    map.get(c.domain).push(c)
  }
  return [...map.entries()]
    .map(([domain, items]) => ({
      domain,
      items: items.slice().sort((a, b) => `${a.name}`.localeCompare(`${b.name}`)),
      expired: items.filter(c => isExpired(c.expires)).length
    }))
    .sort((a, b) => a.domain.localeCompare(b.domain))
})

const allCollapsed = computed(() =>
  domainGroups.value.length > 0 && domainGroups.value.every(g => collapsed[g.domain])
)

function toggleGroup(domain) {
  collapsed[domain] = !collapsed[domain]
}
function collapseAll() {
  domainGroups.value.forEach(g => { collapsed[g.domain] = true })
}
function expandAll() {
  domainGroups.value.forEach(g => { collapsed[g.domain] = false })
}

function expireText(exp) {
  const e = Number(exp) || 0
  if (!e) return '会话级'
  const d = new Date(e * 1000)
  if (isNaN(d.getTime())) return '会话级'
  return (isExpired(e) ? '已于 ' : '有效至 ') + d.toLocaleString()
}

async function load() {
  loading.value = true
  try {
    const res = await botBrowserCookieApi.list(props.botId)
    cookies.value = (res.cookies || []).map(c => ({ ...c, _revealed: false, _plain: '' }))
  } catch (e) {
    MessagePlugin.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

async function fetchPlain(c) {
  if (c._plain) return c._plain
  const full = await botBrowserCookieApi.get(props.botId, c.id, true)
  c._plain = full.value || ''
  return c._plain
}

async function toggleReveal(c) {
  if (c._revealed) {
    c._revealed = false
    return
  }
  try {
    await fetchPlain(c)
    c._revealed = true
  } catch (e) {
    MessagePlugin.error(e.message || '查看失败')
  }
}

async function copyValue(c) {
  try {
    const plain = await fetchPlain(c)
    await navigator.clipboard.writeText(plain)
    MessagePlugin.success('已复制该 Cookie 值')
  } catch (e) {
    MessagePlugin.error(e.message || '复制失败')
  }
}

function resetForm() {
  Object.assign(form, {
    domain: '', name: '', value: '', path: '/', expires: '0',
    httpOnly: false, secure: false, sameSite: ''
  })
}

function openAdd() {
  editing.value = null
  resetForm()
  editVisible.value = true
}

function openEdit(c) {
  editing.value = c.id
  Object.assign(form, {
    domain: c.domain, name: c.name, value: c._revealed ? c._plain : '',
    path: c.path, expires: String(c.expires || 0),
    httpOnly: c.httpOnly, secure: c.secure, sameSite: c.sameSite
  })
  editVisible.value = true
}

async function save() {
  if (!form.domain.trim() || !form.name.trim()) {
    MessagePlugin.warning('域名和名称必填')
    return false
  }
  saving.value = true
  try {
    const payload = {
      domain: form.domain.trim(),
      name: form.name.trim(),
      value: form.value,
      path: form.path || '/',
      expires: parseInt(form.expires, 10) || 0,
      httpOnly: form.httpOnly,
      secure: form.secure,
      sameSite: form.sameSite
    }
    if (editing.value) {
      await botBrowserCookieApi.update(props.botId, editing.value, payload)
    } else {
      await botBrowserCookieApi.create(props.botId, payload)
    }
    editVisible.value = false
    await load()
    MessagePlugin.success('已保存')
  } catch (e) {
    MessagePlugin.error(e.message || '保存失败')
    return false
  } finally {
    saving.value = false
  }
}

function confirmRemove(c) {
  const dlg = DialogPlugin.confirm({
    header: '删除 Cookie',
    body: `确认删除「${c.domain} / ${c.name}」？`,
    theme: 'warning',
    onConfirm: async () => {
      try {
        await botBrowserCookieApi.remove(props.botId, c.id)
        dlg.destroy()
        await load()
        MessagePlugin.success('已删除')
      } catch (e) {
        MessagePlugin.error(e.message || '删除失败')
      }
    }
  })
}

function confirmClearDomain(domain) {
  const dlg = DialogPlugin.confirm({
    header: '清空该域名 Cookie',
    body: `确认清空「${domain}」下的全部 Cookie？此操作不可撤销。`,
    theme: 'warning',
    onConfirm: async () => {
      try {
        await botBrowserCookieApi.clear(props.botId, domain)
        dlg.destroy()
        await load()
        MessagePlugin.success('已清空该域名')
      } catch (e) {
        MessagePlugin.error(e.message || '清空失败')
      }
    }
  })
}

function confirmClear() {
  const dlg = DialogPlugin.confirm({
    header: '清空全部 Cookie',
    body: '确认清空该 Bot 的全部浏览器 Cookie？此操作不可撤销。',
    theme: 'warning',
    onConfirm: async () => {
      try {
        await botBrowserCookieApi.clear(props.botId)
        dlg.destroy()
        await load()
        MessagePlugin.success('已清空')
      } catch (e) {
        MessagePlugin.error(e.message || '清空失败')
      }
    }
  })
}

function openImport() {
  importText.value = ''
  importClear.value = false
  importDomain.value = ''
  importVisible.value = true
}

async function confirmImport() {
  if (!importText.value.trim()) {
    MessagePlugin.warning('请粘贴内容')
    return false
  }
  if (detected.value.needDomain && !importDomain.value.trim()) {
    MessagePlugin.warning('该格式不含域名，请填写「适用域名」')
    return false
  }
  importing.value = true
  try {
    const res = await botBrowserCookieApi.import(props.botId, {
      raw: importText.value,
      clear: importClear.value,
      domain: importDomain.value.trim()
    })
    importVisible.value = false
    await load()
    MessagePlugin.success(`已导入 ${res.imported || 0} 条`)
  } catch (e) {
    MessagePlugin.error(e.message || '导入失败')
    return false
  } finally {
    importing.value = false
  }
}

async function doExport() {
  try {
    const data = await botBrowserCookieApi.export(props.botId)
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `browser-cookies-${props.botId}.json`
    a.click()
    URL.revokeObjectURL(url)
    MessagePlugin.success('已导出')
  } catch (e) {
    MessagePlugin.error(e.message || '导出失败')
  }
}

onMounted(load)
</script>

<style scoped>
.browser-cookie { padding: 4px 0; }

/* 顶部 */
.bc-toolbar { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; margin-bottom: 12px; flex-wrap: wrap; }
.bc-toolbar-left { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 6px; }
.bc-hint { display: flex; align-items: flex-start; gap: 6px; color: var(--bp-label-tertiary); font-size: 12px; line-height: 1.5; }
.bc-hint :deep(.t-icon) { margin-top: 2px; color: var(--bp-warning); flex-shrink: 0; }
.bc-stats { display: flex; align-items: center; gap: 6px; font-size: 12px; color: var(--bp-label-tertiary); }
.bc-stats b { color: var(--bp-label); font-weight: 600; }
.bc-stats .stat-warn b { color: var(--bp-warning); }
.bc-stats .stat-sep { color: var(--bp-separator); }
.bc-actions { display: flex; gap: 8px; flex-shrink: 0; }

/* 搜索 */
.bc-searchbar { display: flex; align-items: center; gap: 8px; margin-bottom: 12px; }
.bc-search { max-width: 320px; }

/* 空态 / 加载 */
.bc-loading, .bc-empty { padding: 44px 0; text-align: center; color: var(--bp-label-quaternary); }
.bc-empty .empty-icon { width: 56px; height: 56px; border-radius: 50%; background: var(--bp-bg); display: inline-flex; align-items: center; justify-content: center; color: var(--bp-label-quaternary); margin-bottom: 8px; }
.bc-empty .empty-title { font-weight: 600; color: var(--bp-label-secondary); margin-bottom: 6px; }
.bc-empty .empty-desc { font-size: 13px; line-height: 1.7; }
.bc-empty .empty-ops { margin-top: 14px; display: flex; gap: 8px; justify-content: center; }

/* 分组 */
.bc-groups { display: flex; flex-direction: column; gap: 12px; }
.bc-group { border: var(--bp-hairline); border-radius: 10px; overflow: hidden; background: var(--bp-surface); }
.bc-group-head { display: flex; align-items: center; gap: 8px; padding: 10px 12px; background: var(--bp-bg-subtle); border-bottom: var(--bp-hairline); cursor: pointer; user-select: none;
  transition: background var(--bp-duration) var(--bp-ease-out), transform var(--bp-duration) var(--bp-ease-out); }
.bc-group-head:hover { background: var(--bp-surface-fill-hover); }
.bc-group-head:active { transform: scale(var(--bp-press-scale)); }
.gh-caret { color: var(--bp-label-tertiary); flex-shrink: 0; }
.gh-domain { font-size: 13px; font-weight: 600; color: var(--bp-label); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.gh-count { font-size: 11px; color: var(--bp-label-tertiary); background: var(--bp-surface-fill); border-radius: 9px; padding: 1px 7px; flex-shrink: 0; }
.gh-spacer { flex: 1; }
.gh-clear { opacity: 0; transition: opacity var(--bp-duration) var(--bp-ease-out); }
.bc-group-head:hover .gh-clear { opacity: 1; }

/* 条目 */
.bc-list { list-style: none; margin: 0; padding: 0; }
.bc-item { display: flex; align-items: flex-start; gap: 12px; padding: 11px 14px; border-bottom: var(--bp-hairline); }
.bc-item:last-child { border-bottom: none; }
.bc-item:hover { background: var(--bp-bg-subtle); }
.bc-item.is-expired { background: var(--bp-warning-soft); }
.bc-main { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 4px; }
.bc-line1 { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.bc-name { font-size: 13px; font-weight: 600; color: var(--bp-label); word-break: break-all; }
.bc-value { display: flex; align-items: center; gap: 8px; font-size: 12px; color: var(--bp-label-tertiary); }
.bc-value-text { font-family: ui-monospace, Menlo, Consolas, monospace; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 420px; }
.bc-value.revealed .bc-value-text { color: var(--bp-label); white-space: normal; word-break: break-all; max-width: 100%; background: var(--bp-bg-subtle); padding: 4px 6px; border-radius: 4px; }
.bc-linkbtn { border: none; background: none; color: var(--bp-accent); cursor: pointer; font-size: 12px; padding: 0; flex-shrink: 0; }
.bc-linkbtn:hover { text-decoration: underline; }
.bc-sub { display: flex; align-items: center; gap: 12px; }
.bc-sub-item { font-size: 11px; color: var(--bp-label-quaternary); }
.bc-ops { display: flex; gap: 2px; flex-shrink: 0; }
</style>

<!-- dialog body 挂 body 外层，scoped 命中不了，故此处用非 scoped 兜底 -->
<style>
.browser-edit-dialog .t-dialog__body { padding-top: 16px; }
.browser-edit-dialog .bc-form .t-form__controls-content { flex-direction: column; align-items: stretch; }
.browser-import-dialog .t-dialog__body { max-height: 70vh; overflow-y: auto; }
.browser-import-dialog .bc-field { margin-bottom: 14px; }
.browser-import-dialog .lbl { display: block; font-size: 13px; color: var(--bp-label-secondary); margin-bottom: 6px; }
.browser-import-dialog .lbl .req { color: var(--bp-danger); font-size: 12px; margin-left: 2px; }
.browser-import-dialog .bc-help { font-size: 12px; color: var(--bp-label-tertiary); margin-top: 6px; line-height: 1.5; }
.browser-import-dialog .bc-imp-tip { display: flex; gap: 8px; padding: 10px 12px; background: var(--bp-accent-soft); border: var(--bp-hairline); border-radius: 8px; font-size: 12px; color: var(--bp-label-secondary); line-height: 1.6; margin-bottom: 14px; }
.browser-import-dialog .bc-imp-tip .t-icon { color: var(--bp-accent); flex-shrink: 0; margin-top: 2px; }
.browser-import-dialog .bc-imp-tip b { color: var(--bp-label); }
.browser-import-dialog .bc-detect { display: flex; align-items: center; gap: 8px; margin-top: 8px; min-height: 22px; }
.browser-import-dialog .bc-detect .dt-desc { font-size: 12px; color: var(--bp-label-tertiary); }
.browser-import-dialog .bc-detect .dt-muted { font-size: 12px; color: var(--bp-label-quaternary); }
.browser-import-dialog textarea { font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 12px; }
</style>
