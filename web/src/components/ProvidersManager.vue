<template>
  <div class="providers" data-testid="providers-manager">
    <!-- 中间：服务商列表 -->
    <div class="prov-list" data-testid="provider-list">
      <button
        v-for="p in providers"
        :key="p.id"
        class="prov-item"
        :class="{ active: activeId === p.id }"
        :data-testid="`provider-item-${p.id}`"
        @click="activeId = p.id"
      >
        <span class="prov-avatar">{{ initial(p.name) }}</span>
        <span class="prov-name">{{ p.name }}</span>
        <span class="prov-dot" :class="{ on: p.enabled }" :title="p.enabled ? '已启用' : '未启用'" />
      </button>
      <button class="prov-add" data-testid="provider-add" @click="addProvider">
        <t-icon name="add" /> 添加服务商
      </button>
    </div>

    <!-- 右侧：详情 -->
    <div v-if="active" class="prov-detail" data-testid="provider-detail">
      <div class="detail-head">
        <div class="head-left">
          <span class="prov-avatar lg">{{ initial(active.name) }}</span>
          <h3 class="detail-title">{{ active.name }}</h3>
        </div>
        <div class="head-right">
          <span class="enable-label">启用</span>
          <t-switch v-model="active.enabled" data-testid="provider-enable" @change="saveProvider" />
        </div>
      </div>

      <!-- 基本信息表单 -->
      <div class="detail-form">
        <div class="form-grid">
          <div class="field">
            <label>Name</label>
            <t-input v-model="active.name" data-testid="provider-name" placeholder="服务商名称" />
          </div>
          <div class="field">
            <label>Client Type</label>
            <t-select v-model="active.clientType" :options="clientTypes" data-testid="provider-clienttype" />
          </div>
        </div>
        <div class="field">
          <label>Base URL</label>
          <t-input v-model="active.baseUrl" data-testid="provider-baseurl" placeholder="https://api.example.com/v1" />
        </div>
        <div class="field">
          <label class="req">API Key</label>
          <t-input v-model="apiKeyInput" type="password" data-testid="provider-apikey" :placeholder="active.apiKey ? '已配置（留空不修改）' : 'Enter API key'" />
        </div>

        <div class="form-actions">
          <t-button variant="outline" data-testid="provider-test" :loading="testing" @click="testConn">
            <template #icon><t-icon name="refresh" /></template>
            Test Connection
          </t-button>
          <div class="actions-right">
            <t-button variant="outline" theme="danger" data-testid="provider-delete" @click="removeProvider">
              <t-icon name="delete" />
            </t-button>
            <t-button theme="primary" data-testid="provider-save" @click="saveProvider">Save Changes</t-button>
          </div>
        </div>
      </div>

      <!-- 模型区 -->
      <div class="models-head">
        <h4 class="models-title">Models</h4>
        <div class="models-ops">
          <t-button variant="outline" size="small" data-testid="models-import" :loading="importing" @click="importModels">
            <template #icon><t-icon name="download" /></template>
            Import Models
          </t-button>
          <t-button theme="primary" size="small" data-testid="models-add" @click="openAddModel">
            <template #icon><t-icon name="add" /></template>
            Add Model
          </t-button>
        </div>
      </div>

      <t-input v-model="modelSearch" class="model-search" data-testid="models-search" placeholder="Search models...">
        <template #prefix-icon><t-icon name="search" /></template>
      </t-input>

      <div class="model-grid" data-testid="model-grid">
        <div
          v-for="m in filteredModels"
          :key="m.id"
          class="model-card"
          :data-testid="`model-card-${m.id}`"
        >
          <div class="model-top">
            <span class="model-name">{{ m.name }}</span>
            <span v-for="c in m.capabilities" :key="c" class="cap-tag">{{ c }}</span>
          </div>
          <div class="model-bottom">
            <span v-if="m.multimodal" class="badge multimodal" title="多模态">👁</span>
            <span class="badge key" title="需要密钥">🔑</span>
            <span v-if="m.contextLength" class="ctx-tag">{{ ctxLabel(m.contextLength) }}</span>
            <div class="model-card-ops">
              <t-icon name="refresh" class="op" title="刷新" @click="$forceUpdate()" />
              <t-icon name="setting" class="op" :data-testid="`model-edit-${m.id}`" title="设置" @click="openEditModel(m)" />
              <t-icon name="delete" class="op" :data-testid="`model-delete-${m.id}`" title="删除" @click="removeModel(m)" />
            </div>
          </div>
        </div>
        <div v-if="!filteredModels.length" class="model-empty">暂无模型，点击 Add Model 添加</div>
      </div>
    </div>

    <div v-else class="prov-empty">请选择左侧服务商，或添加一个新的服务商</div>

    <!-- 模型新增/编辑弹窗 -->
    <t-dialog
      v-model:visible="modelDialog.visible"
      :header="modelDialog.isEdit ? '编辑模型' : '新增模型'"
      :on-confirm="submitModel"
      width="520px"
      data-testid="model-dialog"
    >
      <t-form :data="modelDialog.form" label-align="top">
        <t-form-item label="模型 ID" v-if="!modelDialog.isEdit">
          <t-input v-model="modelDialog.form.id" placeholder="如 gpt-4o" data-testid="model-form-id" />
        </t-form-item>
        <t-form-item label="显示名称">
          <t-input v-model="modelDialog.form.name" placeholder="如 GPT-4o" data-testid="model-form-name" />
        </t-form-item>
        <t-form-item label="能力">
          <t-checkbox-group v-model="modelDialog.form.caps" data-testid="model-form-caps">
            <t-checkbox v-for="opt in CAP_OPTIONS" :key="opt.value" :value="opt.value">{{ opt.label }}</t-checkbox>
          </t-checkbox-group>
        </t-form-item>
        <div class="dlg-row2">
          <t-form-item label="上下文长度">
            <t-input-number v-model="modelDialog.form.contextLength" :min="0" :step="1000" style="width: 100%" />
          </t-form-item>
          <t-form-item label="Max Tokens">
            <t-input-number v-model="modelDialog.form.maxTokens" :min="1" :step="512" style="width: 100%" />
          </t-form-item>
        </div>
      </t-form>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { providerApi } from '@/api/services'

const providers = ref([])
const activeId = ref('')
const apiKeyInput = ref('')
const testing = ref(false)
const importing = ref(false)
const modelSearch = ref('')

const clientTypes = [
  { label: 'OpenAI Compatible', value: 'OpenAI Compatible' },
  { label: 'OpenAI Responses', value: 'OpenAI Responses' },
  { label: 'Anthropic', value: 'Anthropic' },
  { label: 'Gemini', value: 'Gemini' }
]

const active = computed(() => providers.value.find(p => p.id === activeId.value))

const filteredModels = computed(() => {
  const list = active.value?.models || []
  const q = modelSearch.value.trim().toLowerCase()
  if (!q) return list
  return list.filter(m => (m.name + m.id).toLowerCase().includes(q))
})

function initial(s) { return (s || '?').charAt(0).toUpperCase() }
function ctxLabel(n) {
  if (n >= 1000) return Math.round(n / 1000) + 'k'
  return String(n)
}

async function load() {
  providers.value = await providerApi.list()
  if (!activeId.value && providers.value.length) activeId.value = providers.value[0].id
  apiKeyInput.value = ''
}
onMounted(load)

async function addProvider() {
  const p = await providerApi.create({ name: '新服务商', clientType: 'OpenAI Compatible' })
  await load()
  activeId.value = p.id
  MessagePlugin.success('已添加服务商')
}

async function saveProvider() {
  if (!active.value) return
  try {
    const payload = {
      name: active.value.name,
      clientType: active.value.clientType,
      baseUrl: active.value.baseUrl,
      enabled: active.value.enabled
    }
    if (apiKeyInput.value) payload.apiKey = apiKeyInput.value
    await providerApi.update(active.value.id, payload)
    apiKeyInput.value = ''
    await load()
    MessagePlugin.success('已保存')
  } catch (e) {
    MessagePlugin.error(e.message || '保存失败')
  }
}

function removeProvider() {
  const p = active.value
  const dlg = DialogPlugin.confirm({
    header: '删除服务商',
    body: `确认删除「${p.name}」及其下所有模型？`,
    theme: 'warning',
    onConfirm: async () => {
      await providerApi.remove(p.id)
      dlg.destroy()
      activeId.value = ''
      await load()
      MessagePlugin.success('已删除')
    }
  })
}

async function testConn() {
  testing.value = true
  try {
    const r = await providerApi.test(active.value.id)
    if (r.ok) MessagePlugin.success(`${r.message}${r.latencyMs ? '（' + r.latencyMs + 'ms）' : ''}`)
    else MessagePlugin.warning(r.message || '连接失败')
  } catch (e) {
    MessagePlugin.error(e.message || '测试失败')
  } finally {
    testing.value = false
  }
}

async function importModels() {
  importing.value = true
  try {
    const added = await providerApi.importModels(active.value.id)
    await load()
    MessagePlugin.success(added.length ? `导入 ${added.length} 个模型` : '没有新模型')
  } catch (e) {
    MessagePlugin.error(e.message || '导入失败')
  } finally {
    importing.value = false
  }
}

const CAP_OPTIONS = [
  { value: 'chat', label: '对话 (chat)' },
  { value: 'vision', label: '视觉 (vision)' },
  { value: 'tool', label: '工具调用 (tool)' },
  { value: 'reasoning', label: '推理 (reasoning)' },
  { value: 'embedding', label: '向量 (embedding)' }
]

const modelDialog = ref({ visible: false, isEdit: false, form: {} })
function openAddModel() {
  modelDialog.value = { visible: true, isEdit: false, form: { id: '', name: '', caps: ['chat'], contextLength: 0, maxTokens: 4096, multimodal: false } }
}
function openEditModel(m) {
  modelDialog.value = { visible: true, isEdit: true, form: { ...m, caps: [...(m.capabilities || [])] } }
}
async function submitModel() {
  const f = modelDialog.value.form
  const caps = Array.isArray(f.caps) ? f.caps : []
  const payload = { id: f.id, name: f.name || f.id, capabilities: caps, contextLength: f.contextLength, maxTokens: f.maxTokens, multimodal: caps.includes('vision') }
  try {
    if (modelDialog.value.isEdit) {
      await providerApi.updateModel(active.value.id, f.id, payload)
      MessagePlugin.success('模型已更新')
    } else {
      if (!f.id) return MessagePlugin.warning('请填写模型 ID')
      await providerApi.addModel(active.value.id, payload)
      MessagePlugin.success('模型已添加')
    }
    modelDialog.value.visible = false
    await load()
  } catch (e) {
    MessagePlugin.error(e.message || '操作失败')
  }
}
function removeModel(m) {
  const dlg = DialogPlugin.confirm({
    header: '删除模型',
    body: `确认删除模型「${m.name}」？`,
    theme: 'warning',
    onConfirm: async () => {
      await providerApi.removeModel(active.value.id, m.id)
      dlg.destroy()
      await load()
      MessagePlugin.success('已删除')
    }
  })
}
</script>

<style scoped>
.providers {
  display: flex;
  height: 100%;
  min-height: 480px;
}
/* 服务商列表 */
.prov-list {
  width: 220px;
  flex-shrink: 0;
  border-right: 1px solid #ececec;
  padding: 12px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.prov-item {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 9px 10px;
  border: 1px solid transparent;
  background: transparent;
  border-radius: 10px;
  cursor: pointer;
  font-size: 14px;
  color: #333;
  text-align: left;
}
.prov-item:hover { background: #f3f3f5; }
.prov-item.active { background: #fff; border-color: #d0d0d0; box-shadow: 0 1px 4px rgba(0,0,0,0.08); font-weight: 600; }
.prov-avatar {
  width: 26px; height: 26px; flex-shrink: 0;
  border-radius: 7px; background: #e8eaf0; color: #555;
  display: flex; align-items: center; justify-content: center;
  font-size: 13px; font-weight: 600;
}
.prov-avatar.lg { width: 34px; height: 34px; font-size: 15px; }
.prov-name { flex: 1; min-width: 0; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.prov-dot { width: 8px; height: 8px; border-radius: 50%; background: #ccc; flex-shrink: 0; }
.prov-dot.on { background: #00a870; }
.prov-add {
  margin-top: 6px; padding: 9px; border: 1px dashed #ccc; background: transparent;
  border-radius: 10px; color: #666; cursor: pointer; font-size: 13px;
}
.prov-add:hover { border-color: #0052d9; color: #0052d9; }

/* 详情 */
.prov-detail { flex: 1; overflow-y: auto; padding: 24px 28px; }
.prov-empty { flex: 1; display: flex; align-items: center; justify-content: center; color: #999; }
.detail-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 20px; }
.head-left { display: flex; align-items: center; gap: 12px; }
.detail-title { font-size: 18px; font-weight: 600; margin: 0; }
.head-right { display: flex; align-items: center; gap: 8px; }
.enable-label { font-size: 13px; color: #666; }

.detail-form { margin-bottom: 28px; }
.form-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.field { margin-bottom: 16px; }
.field label { display: block; font-size: 13px; color: #444; margin-bottom: 6px; font-weight: 500; }
.field label.req::after { content: ' *'; color: #d63c3c; }
.form-actions { display: flex; align-items: center; justify-content: space-between; margin-top: 4px; }
.actions-right { display: flex; gap: 8px; }

.models-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 12px; }
.models-title { font-size: 16px; font-weight: 600; margin: 0; }
.models-ops { display: flex; gap: 8px; }
.model-search { margin-bottom: 16px; }

.model-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
.model-card {
  border: 1px solid #ececec; border-radius: 12px; padding: 14px 16px; background: #fff;
  display: flex; flex-direction: column; gap: 14px; min-height: 96px; justify-content: space-between;
}
.model-top { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.model-name { font-size: 14px; font-weight: 600; color: #1d1d1f; }
.cap-tag { font-size: 11px; padding: 1px 7px; border-radius: 6px; background: #f0f1f3; color: #667085; }
.model-bottom { display: flex; align-items: center; gap: 8px; }
.badge { font-size: 13px; }
.ctx-tag { font-size: 11px; padding: 1px 8px; border-radius: 8px; background: rgba(0,168,112,0.12); color: #00a870; font-weight: 600; }
.model-card-ops { margin-left: auto; display: flex; gap: 10px; }
.model-card-ops .op { color: #99a; cursor: pointer; font-size: 15px; }
.model-card-ops .op:hover { color: #0052d9; }
.model-empty { grid-column: 1 / -1; text-align: center; color: #999; padding: 30px 0; }

.dlg-row2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }

/* 模型弹窗表单间距 */
:deep([data-testid='model-dialog'] .t-form__item) { margin-bottom: 18px; }
:deep([data-testid='model-dialog'] .dlg-row2 .t-form__item) { margin-bottom: 0; }
:deep([data-testid='model-form-caps']) { display: flex; flex-wrap: wrap; gap: 8px 18px; }
:deep([data-testid='model-form-caps'] .t-checkbox) { margin-right: 0; }
</style>
