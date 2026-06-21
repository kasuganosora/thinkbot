<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Plus, Pencil, Trash2, Cpu, X } from 'lucide-vue-next'
import { llmApi } from '@/api/client'
import { useToast } from '@/components/ui'
import {
  TButton, TInput, TSelect, TCheckbox,
  TBadge, TSpinner, TEmpty, TPageHeader, TCard,
} from '@/components/ui'
import type { LLMModel } from '@/types/api'

const toast = useToast()
const models = ref<LLMModel[]>([])
const loading = ref(true)
const saving = ref(false)
const editingId = ref<string | null>(null)
const showCreate = ref(false)

const editForm = ref<Partial<LLMModel>>({})

const providers = [
  { value: 'openai', label: 'OpenAI' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'google', label: 'Google Gemini' },
  { value: 'grok', label: 'Grok (xAI)' },
  { value: 'bigmodel', label: 'BigModel (智谱)' },
]

const providerLabel = (v: string) => providers.find(p => p.value === v)?.label || v

async function load() {
  loading.value = true
  try {
    models.value = await llmApi.list()
  } catch {
    toast.error('加载模型列表失败')
  } finally {
    loading.value = false
  }
}

function startEdit(m: LLMModel) {
  editingId.value = m.id
  editForm.value = { ...m, apiKey: '' }
}

function cancelEdit() {
  editingId.value = null
  editForm.value = {}
}

async function saveEdit() {
  if (!editingId.value) return
  saving.value = true
  try {
    const updates: Record<string, unknown> = {}
    const f = editForm.value
    if (f.provider) updates.provider = f.provider
    if (f.model) updates.model = f.model
    if (f.apiKey) updates.apiKey = f.apiKey
    if (f.baseUrl !== undefined) updates.baseUrl = f.baseUrl
    if (f.chatPath !== undefined) updates.chatPath = f.chatPath
    if (f.temperature !== undefined) updates.temperature = f.temperature
    if (f.maxTokens !== undefined) updates.maxTokens = f.maxTokens
    if (f.multimodal !== undefined) updates.multimodal = f.multimodal

    await llmApi.update(editingId.value, updates)
    editingId.value = null
    editForm.value = {}
    await load()
    toast.success('已保存')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '保存失败')
  } finally {
    saving.value = false
  }
}

async function remove(id: string) {
  if (!confirm(`确认删除模型配置 "${id}"？`)) return
  try {
    await llmApi.delete(id)
    await load()
    toast.success('已删除')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '删除失败')
  }
}

const newForm = ref({
  id: '',
  provider: 'openai',
  model: '',
  apiKey: '',
  baseUrl: '',
  chatPath: '',
  temperature: 0.7,
  maxTokens: 4096,
  multimodal: false,
})

function resetForm() {
  newForm.value = {
    id: '', provider: 'openai', model: '', apiKey: '',
    baseUrl: '', chatPath: '', temperature: 0.7, maxTokens: 4096, multimodal: false,
  }
}

async function create() {
  if (!newForm.value.id.trim()) { toast.warning('请输入配置 ID'); return }
  if (!newForm.value.model.trim()) { toast.warning('请输入模型名称'); return }
  if (!newForm.value.apiKey.trim()) { toast.warning('请输入 API Key'); return }
  saving.value = true
  try {
    await llmApi.create(newForm.value)
    showCreate.value = false
    resetForm()
    await load()
    toast.success('已创建')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '创建失败')
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <TPageHeader title="模型管理" subtitle="配置 LLM 供应商和 API Key">
      <template #actions>
        <TButton @click="showCreate = !showCreate">
          <Plus :size="14" />
          添加模型
        </TButton>
      </template>
    </TPageHeader>

    <!-- Inline create panel -->
    <div v-if="showCreate" class="page-content-narrow create-panel">
      <TCard padding="lg">
        <div class="create-panel-header">
          <h2 class="create-panel-title">添加模型配置</h2>
          <TButton variant="ghost" size="icon" @click="showCreate = false">
            <X :size="16" />
          </TButton>
        </div>
        <div class="form-field">
          <label>配置 ID</label>
          <TInput v-model="newForm.id" placeholder="如 main、light、claude" />
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>供应商</label>
            <TSelect v-model="newForm.provider">
              <option v-for="p in providers" :key="p.value" :value="p.value">{{ p.label }}</option>
            </TSelect>
          </div>
          <div class="form-field">
            <label>模型名称</label>
            <TInput v-model="newForm.model" placeholder="如 gpt-4o" />
          </div>
        </div>
        <div class="form-field">
          <label>API Key</label>
          <TInput v-model="newForm.apiKey" type="password" placeholder="sk-..." />
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>Base URL（可选）</label>
            <TInput v-model="newForm.baseUrl" placeholder="自定义 API 地址" />
          </div>
          <div class="form-field">
            <label>Chat Path（可选）</label>
            <TInput v-model="newForm.chatPath" placeholder="/v1/chat/completions" />
          </div>
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>温度</label>
            <TInput v-model="newForm.temperature" type="number" />
          </div>
          <div class="form-field">
            <label>Max Tokens</label>
            <TInput v-model="newForm.maxTokens" type="number" />
          </div>
        </div>
        <div class="form-field">
          <TCheckbox v-model="newForm.multimodal">多模态模型（支持图片/音频输入）</TCheckbox>
        </div>
        <div class="form-actions">
          <TButton variant="ghost" @click="showCreate = false">取消</TButton>
          <TButton :loading="saving" @click="create">创建</TButton>
        </div>
      </TCard>
    </div>

    <div v-if="loading" class="loading-state"><TSpinner size="lg" /></div>

    <TEmpty v-else-if="models.length === 0 && !showCreate" text="暂无模型配置，点击「添加模型」创建" />

    <div v-else-if="models.length > 0" class="page-content-narrow">
      <div class="model-list">
        <TCard v-for="m in models" :key="m.id" padding="default">
          <!-- View mode -->
          <div v-if="editingId !== m.id">
            <div class="model-header">
              <div class="model-title-row">
                <Cpu :size="16" class="model-icon" />
                <span class="model-id">{{ m.id }}</span>
                <TBadge>{{ providerLabel(m.provider) }}</TBadge>
                <TBadge v-if="m.multimodal" variant="outline">多模态</TBadge>
              </div>
              <div class="model-actions">
                <TButton variant="ghost" size="sm" @click="startEdit(m)">
                  <Pencil :size="13" /> 编辑
                </TButton>
                <TButton variant="ghost" size="sm" @click="remove(m.id)">
                  <Trash2 :size="13" /> 删除
                </TButton>
              </div>
            </div>
            <div class="model-body">
              <div class="model-field">
                <span class="field-label">模型</span>
                <span class="field-value mono">{{ m.model }}</span>
              </div>
              <div class="model-field">
                <span class="field-label">API Key</span>
                <span class="field-value mono">{{ m.apiKey || '-' }}</span>
              </div>
              <div v-if="m.baseUrl" class="model-field">
                <span class="field-label">Base URL</span>
                <span class="field-value mono">{{ m.baseUrl }}</span>
              </div>
              <div v-if="m.chatPath" class="model-field">
                <span class="field-label">Chat Path</span>
                <span class="field-value mono">{{ m.chatPath }}</span>
              </div>
              <div class="model-field">
                <span class="field-label">温度</span>
                <span class="field-value">{{ m.temperature }}</span>
              </div>
              <div class="model-field">
                <span class="field-label">Max Tokens</span>
                <span class="field-value">{{ m.maxTokens || '-' }}</span>
              </div>
            </div>
          </div>

          <!-- Edit mode -->
          <div v-else>
            <div class="edit-header">
              <h3 class="edit-title">编辑 {{ m.id }}</h3>
            </div>
            <div class="form-grid">
              <div class="form-field">
                <label>供应商</label>
                <TSelect v-model="editForm.provider">
                  <option v-for="p in providers" :key="p.value" :value="p.value">{{ p.label }}</option>
                </TSelect>
              </div>
              <div class="form-field">
                <label>模型名称</label>
                <TInput v-model="editForm.model" placeholder="如 gpt-4o" />
              </div>
            </div>
            <div class="form-field">
              <label>API Key（留空不修改）</label>
              <TInput v-model="editForm.apiKey" type="password" placeholder="sk-..." />
            </div>
            <div class="form-grid">
              <div class="form-field">
                <label>Base URL</label>
                <TInput v-model="editForm.baseUrl" placeholder="自定义 API 地址" />
              </div>
              <div class="form-field">
                <label>Chat Path</label>
                <TInput v-model="editForm.chatPath" placeholder="/v1/chat/completions" />
              </div>
            </div>
            <div class="form-grid">
              <div class="form-field">
                <label>温度</label>
                <TInput v-model="editForm.temperature" type="number" />
              </div>
              <div class="form-field">
                <label>Max Tokens</label>
                <TInput v-model="editForm.maxTokens" type="number" />
              </div>
            </div>
            <div class="form-field">
              <TCheckbox v-model="editForm.multimodal">多模态模型（支持图片/音频输入）</TCheckbox>
            </div>
            <div class="form-actions">
              <TButton variant="ghost" @click="cancelEdit">取消</TButton>
              <TButton :loading="saving" @click="saveEdit">保存</TButton>
            </div>
          </div>
        </TCard>
      </div>
    </div>
  </div>
</template>

<style scoped>
@import '@/assets/page-common.css';

.model-list {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}

.model-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 0.875rem;
}

.model-title-row {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}

.model-icon {
  color: var(--muted-foreground);
}

.model-id {
  font-size: 0.9375rem;
  font-weight: 600;
  font-family: var(--font-mono);
}

.model-actions {
  display: flex;
  gap: 0.25rem;
}

.model-body {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 0.5rem 2rem;
}

.model-field {
  display: flex;
  gap: 0.5rem;
  align-items: baseline;
}

.field-label {
  font-size: 0.6875rem;
  color: var(--muted-foreground);
  min-width: 64px;
  flex-shrink: 0;
}

.field-value {
  font-size: 0.8125rem;
  color: var(--foreground);
}
.field-value.mono {
  font-family: var(--font-mono);
  word-break: break-all;
  font-size: 0.75rem;
}

.edit-title {
  font-size: 0.9375rem;
  font-weight: 600;
  margin-bottom: 1rem;
}
</style>
