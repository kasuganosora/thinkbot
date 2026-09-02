<template>
  <div class="sp-wrap" data-testid="search-providers">
    <!-- 左栏：提供方列表 -->
    <aside class="sp-side">
      <div class="side-list">
        <div
          v-for="p in providers"
          :key="p.id"
          class="sp-item"
          :class="{ active: cur && cur.id === p.id }"
          @click="select(p)"
        >
          <span class="sp-icon" :style="{ background: p.color }">{{ p.letter }}</span>
          <span class="sp-meta">
            <span class="sp-name">{{ p.name }}</span>
            <span class="sp-type">{{ typeLabel(p.type) }}</span>
          </span>
          <span v-if="p.enabled" class="sp-on"></span>
        </div>
      </div>
      <div class="side-foot">
        <t-button variant="outline" block @click="openAdd">
          <template #icon><t-icon name="add" /></template>
          添加搜索提供方
        </t-button>
      </div>
    </aside>

    <!-- 右栏：详情 -->
    <section class="sp-main">
      <div v-if="!cur" class="sp-empty">
        <t-icon name="internet" size="28px" />
        <div class="e-desc">从左侧选择一个搜索提供方进行配置</div>
      </div>

      <div v-else class="sp-detail">
        <div class="detail-head">
          <div class="dh-left">
            <span class="sp-icon lg" :style="{ background: cur.color }">{{ cur.letter }}</span>
            <div>
              <span class="dh-name">{{ cur.name }}</span>
              <div class="dh-type">{{ typeLabel(cur.type) }}</div>
            </div>
          </div>
          <div class="dh-right">
            <span class="en-label">启用</span>
            <t-switch v-model="cur.enabled" @change="onToggle" />
          </div>
        </div>
        <t-divider style="margin: 4px 0 16px" />
        <div class="fallback-note">多个启用时按列表顺序 fallback</div>

        <div class="field">
          <label class="lbl">名称</label>
          <t-input v-model="cur.name" placeholder="输入名称" />
        </div>

        <div v-if="fieldShown('apiKey')" class="field">
          <label class="lbl">
            {{ fieldMeta('apiKey').label }}
            <span v-if="fieldMeta('apiKey').required" class="req">*</span>
            <span v-else class="opt">（可选）</span>
          </label>
          <div v-if="fieldMeta('apiKey').help" class="hint">{{ fieldMeta('apiKey').help }}</div>
          <t-input v-model="cur.apiKey" type="password" :placeholder="fieldMeta('apiKey').placeholder" />
        </div>

        <div v-if="fieldShown('searchType') && fieldShown('timeout')" class="grid2">
          <div class="field">
            <label class="lbl">
              {{ fieldMeta('searchType').label }}
              <span v-if="fieldMeta('searchType').required" class="req">*</span>
              <span v-else class="opt">（可选）</span>
            </label>
            <div v-if="fieldMeta('searchType').help" class="hint">{{ fieldMeta('searchType').help }}</div>
            <t-select
              v-if="schema.searchTypeOptions"
              v-model="cur.searchType"
              :options="searchTypeSelectOptions"
              :placeholder="fieldMeta('searchType').placeholder"
            />
            <t-input
              v-else
              v-model="cur.searchType"
              :placeholder="fieldMeta('searchType').placeholder"
            />
          </div>
          <div class="field">
            <label class="lbl">{{ fieldMeta('timeout').label }}</label>
            <div v-if="fieldMeta('timeout').help" class="hint">{{ fieldMeta('timeout').help }}</div>
            <t-input-number v-model="cur.timeout" :min="1" :max="120" style="width: 100%" />
          </div>
        </div>
        <template v-else>
          <div v-if="fieldShown('searchType')" class="field">
            <label class="lbl">
              {{ fieldMeta('searchType').label }}
              <span v-if="fieldMeta('searchType').required" class="req">*</span>
              <span v-else class="opt">（可选）</span>
            </label>
            <div v-if="fieldMeta('searchType').help" class="hint">{{ fieldMeta('searchType').help }}</div>
            <t-select
              v-if="schema.searchTypeOptions"
              v-model="cur.searchType"
              :options="searchTypeSelectOptions"
              :placeholder="fieldMeta('searchType').placeholder"
            />
            <t-input
              v-else
              v-model="cur.searchType"
              :placeholder="fieldMeta('searchType').placeholder"
            />
          </div>
          <div v-if="fieldShown('timeout')" class="field">
            <label class="lbl">{{ fieldMeta('timeout').label }}</label>
            <div v-if="fieldMeta('timeout').help" class="hint">{{ fieldMeta('timeout').help }}</div>
            <t-input-number v-model="cur.timeout" :min="1" :max="120" style="width: 100%" />
          </div>
        </template>

        <div v-if="fieldShown('baseUrl') && fieldMeta('baseUrl').required" class="field">
          <label class="lbl">
            {{ fieldMeta('baseUrl').label }}
            <span class="req">*</span>
          </label>
          <div v-if="fieldMeta('baseUrl').help" class="hint">{{ fieldMeta('baseUrl').help }}</div>
          <t-input v-model="cur.baseUrl" :placeholder="fieldMeta('baseUrl').placeholder" />
        </div>
        <div v-else-if="fieldShown('baseUrl')" class="advanced">
          <button type="button" class="adv-toggle" @click="showAdvanced = !showAdvanced">
            高级
            <t-icon :name="showAdvanced ? 'chevron-up' : 'chevron-down'" size="16px" />
          </button>
          <div v-if="showAdvanced" class="field adv-body">
            <label class="lbl">
              {{ fieldMeta('baseUrl').label }}
              <span class="opt">（可选）</span>
            </label>
            <div v-if="fieldMeta('baseUrl').help" class="hint">{{ fieldMeta('baseUrl').help }}</div>
            <t-input v-model="cur.baseUrl" :placeholder="fieldMeta('baseUrl').placeholder" />
          </div>
        </div>

        <div class="detail-foot">
          <t-button variant="outline" shape="square" @click="remove">
            <t-icon name="delete" />
          </t-button>
          <t-button theme="default" :loading="saving" @click="save">保存修改</t-button>
        </div>
      </div>
    </section>

    <!-- 添加弹窗 -->
    <t-dialog
      v-model:visible="addVisible"
      header="添加搜索提供方"
      :width="520"
      :confirm-btn="{ content: '确认', disabled: !addCanSubmit }"
      :on-confirm="confirmAdd"
      dialogClassName="sp-add-dialog"
    >
      <div class="add-form">
        <div class="field">
          <label class="lbl">提供方类型</label>
          <t-select v-model="addForm.type" :options="typeOptions" placeholder="选择类型" />
        </div>
        <div v-if="addForm.type && addRequiredTip" class="add-tip">
          <t-icon name="info-circle" size="14px" />
          <span>{{ addRequiredTip }}</span>
        </div>
        <template v-if="addForm.type">
          <!-- 必填字段：直接展示 -->
          <template v-for="key in ['apiKey', 'searchType', 'baseUrl']" :key="'req-' + key">
            <div v-if="addFieldMeta(key).show && addFieldMeta(key).required" class="field">
              <label class="lbl">
                {{ addFieldMeta(key).label }}
                <span class="req">*</span>
              </label>
              <div v-if="addFieldMeta(key).help" class="hint">{{ addFieldMeta(key).help }}</div>
              <t-input v-if="key === 'apiKey'" v-model="addForm.apiKey" type="password" :placeholder="addFieldMeta(key).placeholder" />
              <t-select v-else-if="key === 'searchType' && addSchema.searchTypeOptions" v-model="addForm.searchType" :options="addSchema.searchTypeOptions" :placeholder="addFieldMeta(key).placeholder" />
              <t-input v-else v-model="addForm[key]" :placeholder="addFieldMeta(key).placeholder" />
            </div>
          </template>
          <!-- 可选字段：折叠，需要时展开填写 -->
          <div v-if="addHasOptional" class="advanced">
            <button type="button" class="adv-toggle" @click="showAddAdvanced = !showAddAdvanced">
              可选配置
              <t-icon :name="showAddAdvanced ? 'chevron-up' : 'chevron-down'" size="16px" />
            </button>
            <template v-if="showAddAdvanced">
              <template v-for="key in ['apiKey', 'searchType', 'baseUrl']" :key="'opt-' + key">
                <div v-if="addFieldMeta(key).show && !addFieldMeta(key).required" class="field adv-field">
                  <label class="lbl">
                    {{ addFieldMeta(key).label }}
                    <span class="opt">（可选）</span>
                  </label>
                  <div v-if="addFieldMeta(key).help" class="hint">{{ addFieldMeta(key).help }}</div>
                  <t-input v-if="key === 'apiKey'" v-model="addForm.apiKey" type="password" :placeholder="addFieldMeta(key).placeholder" />
                  <t-select v-else-if="key === 'searchType' && addSchema.searchTypeOptions" v-model="addForm.searchType" :options="addSchema.searchTypeOptions" :placeholder="addFieldMeta(key).placeholder" />
                  <t-input v-else v-model="addForm[key]" :placeholder="addFieldMeta(key).placeholder" />
                </div>
              </template>
            </template>
          </div>
        </template>
        <div class="field">
          <label class="lbl">名称</label>
          <t-input v-model="addForm.name" placeholder="输入名称" @enter="confirmAdd" />
        </div>
      </div>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, computed, watch } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { searchProviderApi, SEARCH_PROVIDER_TYPES, searchProviderSchema } from '@/api/services'

const providers = ref([])
const cur = ref(null)
const saving = ref(false)
const showAdvanced = ref(false)

const typeOptions = computed(() =>
  searchProviderApi.types().map(t => ({ label: t.label, value: t.type }))
)

const schema = computed(() => searchProviderSchema(cur.value?.type))

const searchTypeSelectOptions = computed(() => {
  const opts = schema.value.searchTypeOptions || []
  const val = cur.value?.searchType
  if (val && !opts.some(o => o.value === val)) return [...opts, { value: val, label: val }]
  return opts
})

function typeLabel(type) {
  return searchProviderSchema(type).label || type
}
function fieldMeta(key) {
  return schema.value.fields?.[key] || { show: false, required: false, label: '', placeholder: '', help: '' }
}
function fieldShown(key) {
  return !!fieldMeta(key).show
}

function applyTypeDefaults(p) {
  if (!p) return
  if (p.type === 'yandex' && !String(p.searchType || '').trim()) p.searchType = 'SEARCH_TYPE_RU'
  const f = searchProviderSchema(p.type).fields?.baseUrl
  showAdvanced.value = !!(p.baseUrl && f?.show && !f.required)
}

async function load(selectId) {
  const res = await searchProviderApi.list()
  providers.value = res.providers || []
  if (selectId) cur.value = providers.value.find(p => p.id === selectId) || null
  else if (cur.value) cur.value = providers.value.find(p => p.id === cur.value.id) || null
  else cur.value = providers.value[0] || null
  applyTypeDefaults(cur.value)
}
function select(p) {
  cur.value = p
  applyTypeDefaults(p)
}

async function onToggle(val) {
  try {
    await searchProviderApi.toggle(cur.value.id, val)
    const item = providers.value.find(p => p.id === cur.value.id)
    if (item) item.enabled = val
    MessagePlugin.success(val ? '已启用' : '已停用')
  } catch (e) {
    cur.value.enabled = !val
    MessagePlugin.error(e.message || '操作失败')
  }
}

function missingRequired(p) {
  const s = searchProviderSchema(p.type)
  const labels = []
  for (const key of ['apiKey', 'searchType', 'baseUrl']) {
    const f = s.fields?.[key]
    if (!f?.show || !f.required) continue
    const raw = String(p[key] ?? '').trim()
    const invalidPlaceholder = key === 'searchType' && p.type !== 'yandex' && /^SEARCH_TYPE_/i.test(raw)
    if (!raw || invalidPlaceholder) labels.push(f.label)
  }
  return labels
}

async function save() {
  const missing = missingRequired(cur.value)
  if (missing.length) {
    MessagePlugin.warning(`请填写必填项：${missing.join('、')}`)
    return
  }
  saving.value = true
  try {
    await searchProviderApi.update(cur.value.id, {
      name: cur.value.name, apiKey: cur.value.apiKey,
      searchType: cur.value.searchType, timeout: cur.value.timeout,
      baseUrl: cur.value.baseUrl, enabled: cur.value.enabled
    })
    await load(cur.value.id)
    MessagePlugin.success('已保存')
  } catch (e) {
    MessagePlugin.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

function remove() {
  const target = cur.value
  const dlg = DialogPlugin.confirm({
    header: '删除搜索提供方', body: `确认删除「${target.name}」？`, theme: 'warning',
    onConfirm: async () => {
      await searchProviderApi.remove(target.id)
      dlg.destroy()
      cur.value = null
      await load()
      MessagePlugin.success('已删除')
    }
  })
}

/* 添加 */
const addVisible = ref(false)
const addForm = reactive({ name: '', type: '', apiKey: '', searchType: '', baseUrl: '' })
const addSchema = computed(() => (addForm.type ? searchProviderSchema(addForm.type) : null))
const addRequiredTip = computed(() => addSchema.value?.requiredTip || '')
const showAddAdvanced = ref(false)
function addFieldMeta(key) {
  return addSchema.value?.fields?.[key] || { show: false, required: false, label: '', placeholder: '', help: '' }
}
// 有无可折叠的可选字段（show 但非 required）
const addHasOptional = computed(() =>
  ['apiKey', 'searchType', 'baseUrl'].some(k => addFieldMeta(k).show && !addFieldMeta(k).required)
)
const addCanSubmit = computed(() => {
  if (!addForm.name.trim() || !addForm.type) return false
  for (const key of ['apiKey', 'searchType', 'baseUrl']) {
    if (addFieldMeta(key).required && !String(addForm[key] || '').trim()) return false
  }
  return true
})
function openAdd() {
  addForm.name = ''; addForm.type = ''
  addForm.apiKey = ''; addForm.searchType = ''; addForm.baseUrl = ''
  showAddAdvanced.value = false
  addVisible.value = true
}
watch(() => addForm.type, (type) => {
  if (!type) return
  const meta = searchProviderSchema(type)
  const known = SEARCH_PROVIDER_TYPES.map(t => t.label)
  if (!addForm.name.trim() || known.includes(addForm.name.trim())) {
    addForm.name = meta.label
  }
  // 切换类型时清空上一类型填写的密钥类字段并收起可选区
  addForm.apiKey = ''; addForm.searchType = ''; addForm.baseUrl = ''
  showAddAdvanced.value = false
})
async function confirmAdd() {
  if (!addCanSubmit.value) return MessagePlugin.warning('请填写名称、类型及必填配置项')
  const payload = { name: addForm.name.trim(), type: addForm.type }
  if (addForm.apiKey.trim()) payload.apiKey = addForm.apiKey.trim()
  if (addForm.searchType.trim()) payload.searchType = addForm.searchType.trim()
  if (addForm.baseUrl.trim()) payload.baseUrl = addForm.baseUrl.trim()
  const created = await searchProviderApi.create(payload)
  addVisible.value = false
  await load(created.id)
  MessagePlugin.success('已添加')
}

load()
</script>

<style scoped>
.sp-wrap { display: flex; height: 100%; min-height: 480px; }

/* 左栏 */
.sp-side {
  width: 240px; flex-shrink: 0; display: flex; flex-direction: column;
  border-right: var(--bp-hairline); background: var(--bp-bg-subtle);
}
.side-list { flex: 1; overflow-y: auto; padding: 12px; display: flex; flex-direction: column; gap: 4px; }
.sp-item {
  display: flex; align-items: center; gap: 10px; padding: 9px 12px;
  border-radius: 10px; cursor: pointer;
}
.sp-item:hover { background: var(--bp-surface-fill-hover); }
.sp-item.active { background: var(--bp-surface); box-shadow: var(--bp-shadow-sm); }
.sp-icon {
  width: 24px; height: 24px; border-radius: 6px; flex-shrink: 0; color: var(--bp-label-on-accent);
  display: flex; align-items: center; justify-content: center; font-size: 13px; font-weight: 700;
}
.sp-icon.lg { width: 32px; height: 32px; border-radius: 8px; font-size: 16px; }
.sp-meta { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 1px; }
.sp-name { font-size: 14px; color: var(--bp-label); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sp-type { font-size: 11px; color: var(--bp-label-tertiary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sp-on { width: 8px; height: 8px; border-radius: 50%; background: var(--bp-success); flex-shrink: 0; }
.side-foot { padding: 12px; border-top: var(--bp-hairline); }

/* 右栏 */
.sp-main { flex: 1; min-width: 0; overflow-y: auto; padding: 24px 32px; }
.sp-empty {
  height: 100%; min-height: 420px; display: flex; flex-direction: column;
  align-items: center; justify-content: center; gap: 10px; color: var(--bp-label-tertiary);
}
.e-desc { font-size: 13px; }

.detail-head { display: flex; align-items: center; justify-content: space-between; }
.dh-left { display: flex; align-items: center; gap: 12px; }
.dh-name { font-size: 20px; font-weight: 700; color: var(--bp-label); }
.dh-type { font-size: 12px; color: var(--bp-label-tertiary); margin-top: 2px; }
.dh-right { display: flex; align-items: center; gap: 10px; }
.en-label { font-size: 13px; color: var(--bp-label-tertiary); }

.fallback-note {
  font-size: 12px; color: var(--bp-label-tertiary); background: var(--bp-surface-fill); border: none; box-shadow: var(--bp-shadow-sm);
  border-radius: 8px; padding: 8px 12px; margin-bottom: 18px;
}

.field { display: flex; flex-direction: column; margin-bottom: 18px; }
.lbl { font-size: 13px; font-weight: 600; color: var(--bp-label); margin-bottom: 8px; }
.lbl .req { color: var(--bp-danger); margin-left: 2px; font-weight: 600; }
.lbl .opt { font-weight: 400; color: var(--bp-label-tertiary); font-size: 12px; margin-left: 4px; }
.hint { font-size: 12px; color: var(--bp-label-tertiary); margin: -4px 0 8px; line-height: 1.5; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; }

.advanced { margin-bottom: 12px; }
.adv-toggle {
  display: inline-flex; align-items: center; gap: 4px; border: 0; background: none;
  color: var(--bp-label-secondary); font-size: 13px; cursor: pointer; padding: 0;
}
.adv-toggle:hover { color: var(--bp-label); }
.adv-body { margin-top: 12px; margin-bottom: 0; }

.detail-foot { display: flex; justify-content: flex-end; align-items: center; gap: 12px; margin-top: 8px; }

.add-form .field { margin-bottom: 18px; }
.add-tip {
  display: flex; align-items: flex-start; gap: 6px;
  font-size: 12px; color: var(--bp-label-tertiary);
  margin: -6px 0 16px; line-height: 1.5;
}
.add-tip .t-icon { flex-shrink: 0; margin-top: 2px; }
.add-form .advanced { margin-bottom: 4px; }
.add-form .adv-field { margin-top: 14px; }
</style>

<style>
.sp-add-dialog.t-dialog { padding: 20px 20px 16px; }
.sp-add-dialog .t-dialog__header { padding: 0; margin-bottom: 16px; }
.sp-add-dialog .t-dialog__body { padding: 0; }
.sp-add-dialog .t-dialog__footer { padding: 16px 0 0; }
</style>
