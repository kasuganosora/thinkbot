<template>
  <SettingsShell title="系统配置">
    <t-card :bordered="false" class="card">
      <div class="toolbar">
        <span class="hint">这些配置由后端 /api/config 持久化，按分类分组。修改后点击「保存全部修改」。</span>
        <t-button theme="primary" :disabled="!dirty" @click="saveAll" data-testid="config-save-btn">保存全部修改</t-button>
      </div>

      <t-loading :loading="loading">
        <div v-for="(items, cat) in grouped" :key="cat" class="group">
          <div class="group-title">{{ catLabel(cat) }}</div>
          <t-form label-align="left" :label-width="200">
            <t-form-item v-for="item in items" :key="item.key" :label="item.description || item.key">
              <div class="field">
                <t-switch
                  v-if="isBool(item.value)"
                  :value="item.value === 'true'"
                  @change="v => updateValue(item.key, v ? 'true' : 'false')"
                  :data-testid="`config-${item.key}`"
                />
                <t-input
                  v-else
                  :value="draft[item.key]"
                  @change="v => updateValue(item.key, v)"
                  :data-testid="`config-${item.key}`"
                  style="width: 320px"
                />
                <span class="key-name">{{ item.key }}</span>
              </div>
            </t-form-item>
          </t-form>
        </div>
      </t-loading>
    </t-card>
  </SettingsShell>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import SettingsShell from '@/components/SettingsShell.vue'
import { configApi } from '@/api/services'

const loading = ref(false)
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

const catLabels = { chat: '对话', token: 'Token 配额', system: '系统', workflow: '工作流', other: '其它' }
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
onMounted(load)

function updateValue(key, val) {
  draft.value[key] = val
  changed.value[key] = val
}

async function saveAll() {
  try {
    await configApi.batchSet({ ...changed.value })
    MessagePlugin.success('配置已保存')
    load()
  } catch (e) {
    MessagePlugin.error(e.message || '保存失败')
  }
}
</script>

<style scoped>
.card { margin-bottom: 20px; }
.toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 20px;
}
.hint { color: #888; font-size: 13px; max-width: 70%; }
.group { margin-bottom: 24px; }
.group-title {
  font-weight: 600;
  font-size: 14px;
  margin-bottom: 12px;
  padding-left: 8px;
  border-left: 3px solid var(--td-brand-color, #00a870);
}
.field { display: flex; align-items: center; gap: 12px; }
.key-name { color: #bbb; font-size: 12px; font-family: monospace; }
</style>
