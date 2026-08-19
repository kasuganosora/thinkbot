<template>
  <div class="browser-cookie" data-testid="bot-browser">
    <!-- 顶部操作栏 -->
    <div class="bc-toolbar">
      <div class="bc-hint">
        <t-icon name="info-circle" />
        <span>浏览器 Cookie 即账号凭据。新增/导入后，下次浏览器会话自动注入；删除需下次会话生效。</span>
      </div>
      <div class="bc-actions">
        <t-button theme="primary" @click="openAdd">
          <template #icon><t-icon name="add" /></template>添加
        </t-button>
        <t-button variant="outline" @click="openImport">
          <template #icon><t-icon name="upload" /></template>导入
        </t-button>
        <t-button variant="outline" @click="doExport">
          <template #icon><t-icon name="download" /></template>导出
        </t-button>
        <t-button theme="danger" variant="outline" @click="confirmClear">
          <template #icon><t-icon name="delete" /></template>清空
        </t-button>
      </div>
    </div>

    <!-- 列表 -->
    <div v-if="loading" class="bc-loading"><t-loading /></div>
    <div v-else-if="cookies.length === 0" class="bc-empty">
      <div class="empty-icon"><t-icon name="cookie" size="28px" /></div>
      <div class="empty-title">暂无 Cookie</div>
      <div class="empty-desc">从浏览器导出 storage_state / cookies.txt 粘贴导入，或手动添加。</div>
    </div>
    <ul v-else class="bc-list">
      <li v-for="c in cookies" :key="c.id" class="bc-item">
        <div class="bc-main">
          <div class="bc-domain">{{ c.domain }}</div>
          <div class="bc-name">{{ c.name }}</div>
          <div class="bc-value" :class="{ revealed: c._revealed }">
            <span class="bc-value-text">{{ c._revealed && c._plain ? c._plain : c.value }}</span>
            <button class="bc-reveal" @click="toggleReveal(c)">{{ c._revealed ? '隐藏' : '查看' }}</button>
          </div>
        </div>
        <div class="bc-meta">
          <t-tag v-if="c.httpOnly" size="small" variant="light">HttpOnly</t-tag>
          <t-tag v-if="c.secure" size="small" variant="light" theme="success">Secure</t-tag>
          <t-tag v-if="c.sameSite" size="small" variant="light">{{ c.sameSite }}</t-tag>
          <span class="bc-exp">{{ expireText(c.expires) }}</span>
          <span class="bc-path">path: {{ c.path }}</span>
        </div>
        <div class="bc-ops">
          <t-button size="small" variant="text" @click="openEdit(c)">编辑</t-button>
          <t-button size="small" variant="text" theme="danger" @click="confirmRemove(c)">删除</t-button>
        </div>
      </li>
    </ul>

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
      :width="560"
      confirm-btn="导入"
      :on-confirm="confirmImport"
      dialogClassName="browser-import-dialog"
    >
      <div class="bc-field">
        <label class="lbl">粘贴 storageState JSON / cookies.txt / 扩展导出的 JSON 数组</label>
        <t-textarea v-model="importText" :autosize="{ minRows: 8, maxRows: 16 }" placeholder='{"cookies":[{...}]} 或 # Netscape HTTP Cookie File ...' />
      </div>
      <t-checkbox v-model="importClear">导入前清空现有 Cookie</t-checkbox>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botBrowserCookieApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const cookies = ref([])
const loading = ref(false)
const saving = ref(false)

const editVisible = ref(false)
const editing = ref(null)
const form = reactive({ domain: '', name: '', value: '', path: '/', expires: '0', httpOnly: false, secure: false, sameSite: '' })

const importVisible = ref(false)
const importText = ref('')
const importClear = ref(false)

const sameSiteOptions = [
  { label: '无 (空)', value: '' },
  { label: 'Lax', value: 'Lax' },
  { label: 'Strict', value: 'Strict' },
  { label: 'None', value: 'None' }
]

function expireText(exp) {
  if (!exp || exp === 0) return '会话级'
  const d = new Date(exp * 1000)
  if (isNaN(d.getTime())) return '会话级'
  return d.toLocaleString()
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

async function toggleReveal(c) {
  if (c._revealed) {
    c._revealed = false
    return
  }
  try {
    const full = await botBrowserCookieApi.get(props.botId, c.id, true)
    c._plain = full.value || ''
    c._revealed = true
  } catch (e) {
    MessagePlugin.error(e.message || '查看失败')
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
  importVisible.value = true
}

async function confirmImport() {
  if (!importText.value.trim()) {
    MessagePlugin.warning('请粘贴内容')
    return false
  }
  try {
    const res = await botBrowserCookieApi.import(props.botId, {
      raw: importText.value,
      clear: importClear.value
    })
    importVisible.value = false
    await load()
    MessagePlugin.success(`已导入 ${res.imported || 0} 条`)
  } catch (e) {
    MessagePlugin.error(e.message || '导入失败')
    return false
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
.bc-toolbar { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; margin-bottom: 16px; flex-wrap: wrap; }
.bc-hint { display: flex; align-items: flex-start; gap: 6px; color: #8a8a8a; font-size: 12px; line-height: 1.5; max-width: 60%; }
.bc-hint :deep(.t-icon) { margin-top: 2px; color: #d9a300; }
.bc-actions { display: flex; gap: 8px; flex-shrink: 0; }
.bc-loading, .bc-empty { padding: 48px 0; text-align: center; color: #aaa; }
.bc-empty .empty-icon { width: 56px; height: 56px; border-radius: 50%; background: #f5f5f7; display: inline-flex; align-items: center; justify-content: center; color: #bbb; margin-bottom: 8px; }
.bc-empty .empty-title { font-weight: 600; color: #555; margin-bottom: 4px; }
.bc-empty .empty-desc { font-size: 13px; }

.bc-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
.bc-item { display: flex; align-items: center; gap: 12px; padding: 12px 14px; border: 1px solid #ececec; border-radius: 10px; background: #fff; }
.bc-main { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.bc-domain { font-size: 13px; font-weight: 600; color: #1d1d1f; }
.bc-name { font-size: 12px; color: #555; }
.bc-value { display: flex; align-items: center; gap: 8px; font-size: 12px; color: #888; }
.bc-value-text { font-family: ui-monospace, Menlo, Consolas, monospace; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 360px; }
.bc-value.revealed .bc-value-text { color: #1d1d1f; white-space: normal; word-break: break-all; max-width: 480px; }
.bc-reveal { border: none; background: none; color: #0052d9; cursor: pointer; font-size: 12px; padding: 0; flex-shrink: 0; }
.bc-meta { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
.bc-exp, .bc-path { font-size: 11px; color: #aaa; }
.bc-ops { display: flex; gap: 4px; flex-shrink: 0; }
</style>

<!-- dialog body 挂 body 外层，scoped 命中不了，故此处用非 scoped 兜底 -->
<style>
.browser-edit-dialog .t-dialog__body { padding-top: 16px; }
.browser-edit-dialog .bc-form :deep(.t-form__controls-content) { flex-direction: column; align-items: stretch; }
.browser-import-dialog .t-dialog__body { max-height: 70vh; overflow-y: auto; }
.browser-import-dialog .bc-field { margin-bottom: 12px; }
.browser-import-dialog .lbl { display: block; font-size: 13px; color: #555; margin-bottom: 6px; }
</style>
