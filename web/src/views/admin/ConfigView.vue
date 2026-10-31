<template>
  <!--
    One form body for both shells (SettingsShell + embedded) so layout/CSS cannot drift.
    Descriptions are top-aligned and wrap; technical keys sit beside/under controls.
  -->
  <component
    :is="embedded ? 'div' : SettingsShell"
    v-bind="embedded ? { class: 'embed-panel' } : { title: '系统配置' }"
  >
    <div v-if="embedded" class="panel-head">
      <div>
        <h3 class="panel-title">系统配置</h3>
        <p class="panel-desc">全局系统配置项，按分类分组，修改后点击「保存全部修改」。</p>
      </div>
    </div>

    <component
      :is="embedded ? 'div' : 't-card'"
      v-bind="embedded ? { class: 'panel-card' } : { bordered: false, class: 'card' }"
    >
      <div class="toolbar">
        <span class="hint">这些配置由后端 /api/config 持久化，按分类分组。修改后点击「保存全部修改」。</span>
        <t-button theme="primary" :disabled="!dirty || loading" :loading="saving" @click="saveAll" data-testid="config-save-btn">保存全部修改</t-button>
      </div>

      <t-alert v-if="feedback" :theme="feedback.theme" :message="feedback.message" class="save-feedback" data-testid="config-feedback" />
      <t-loading :loading="loading">
        <div v-for="(items, cat) in grouped" :key="cat" class="group">
          <div class="group-title">{{ catLabel(cat) }}</div>
          <t-form label-align="top" class="config-form">
            <t-form-item v-for="item in items" :key="item.key">
              <template #label>
                <span class="item-label">{{ item.description || item.key }}</span>
              </template>
              <div class="field">
                <t-switch
                  v-if="isBool(item.value)"
                  :disabled="saving"
                  :value="draft[item.key] === 'true'"
                  @change="v => updateValue(item.key, v ? 'true' : 'false')"
                  :data-testid="`config-${item.key}`"
                />
                <t-input
                  v-else
                  :disabled="saving"
                  :value="draft[item.key]"
                  @change="v => updateValue(item.key, v)"
                  :data-testid="`config-${item.key}`"
                  class="config-input"
                />
                <span class="key-name">{{ item.key }}</span>
              </div>
            </t-form-item>
          </t-form>
        </div>
      </t-loading>
    </component>
  </component>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import SettingsShell from '@/components/SettingsShell.vue'
import { configApi } from '@/api/services'
import { configApplyFeedback } from '@/utils/configApplyFeedback'

defineProps({ embedded: { type: Boolean, default: false } })

const loading = ref(false)
const saving = ref(false)
const feedback = ref(null)
const list = ref([])
const draft = ref({})
const changed = ref({})

const dirty = computed(() => Object.keys(changed.value).length > 0)

const grouped = computed(() => {
  const g = {}
  for (const item of list.value) {
    const cat = item.category || 'other'
    if (!g[cat]) g[cat] = []
    g[cat].push(item)
  }
  return g
})

const catLabels = { chat: '对话', token: 'Token 配额', system: '系统', System: '系统', workflow: '工作流', memory_window: '记忆窗口', MemoryWindow: '记忆窗口', llm: 'LLM 客户端', LLM: 'LLM 客户端', compaction: '会话压缩', Compaction: '会话压缩', Engagement: '主动参与', Workspace: '沙箱工作空间', InternalLLM: '内置 LLM', other: '其它' }
function catLabel(c) { return catLabels[c] || c }

function isBool(v) { return v === 'true' || v === 'false' }

async function load() {
  loading.value = true
  try {
    list.value = await configApi.list()
    const d = {}
    list.value.forEach(i => { d[i.key] = i.value })
    draft.value = d
    changed.value = {}
  } finally {
    loading.value = false
  }
}
onMounted(() => load().catch(e => { feedback.value = { theme: 'error', message: e.message || '加载配置失败，请刷新重试' } }))

function updateValue(key, val) {
  feedback.value = null
  draft.value[key] = val
  changed.value[key] = val
}

async function saveAll() {
  if (saving.value || !dirty.value) return
  saving.value = true
  try {
    const result = await configApi.batchSet({ ...changed.value })
    feedback.value = configApplyFeedback(result)
    MessagePlugin[feedback.value.theme](feedback.value.message)
    // The write succeeded even if the subsequent refresh fails.
    changed.value = {}
    try { await load() } catch (e) {
      feedback.value = { theme: 'warning', message: `${feedback.value.message}；刷新失败，请刷新页面确认最新值` }
    }
  } catch (e) {
    feedback.value = { theme: 'error', message: e.message || '保存失败' }
    MessagePlugin.error(feedback.value.message)
  } finally {
    saving.value = false
  }
}
</script>

<style scoped>
.save-feedback { margin-bottom: 16px; }
.card { margin-bottom: 20px; }
.toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  margin-bottom: 20px;
}
.hint { color: var(--bp-label-tertiary); font-size: 13px; max-width: 70%; }
.group { margin-bottom: 24px; }
.group-title {
  font-weight: 600;
  font-size: 14px;
  margin-bottom: 12px;
  padding-left: 8px;
  border-left: 3px solid var(--bp-accent);
}
.field {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px 12px;
  width: 100%;
}
.key-name {
  color: var(--bp-label-quaternary);
  font-size: 12px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
  word-break: break-all;
}
/* Long Chinese descriptions must wrap; TDesign left labels default to nowrap. */
.config-form :deep(.t-form__label),
.config-form :deep(.t-form-item__label),
.item-label {
  white-space: normal;
  word-break: break-word;
  line-height: 1.45;
}
/* Prefer class max-width over inline width so the field can flex. */
.config-input,
.field :deep(.t-input) {
  flex: 1 1 220px;
  max-width: 480px;
  min-width: 0;
  width: 100%;
}
</style>
