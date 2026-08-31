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
          <span class="sp-name">{{ p.name }}</span>
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
            <span class="dh-name">{{ cur.name }}</span>
          </div>
          <div class="dh-right">
            <span class="en-label">启用</span>
            <t-switch v-model="cur.enabled" @change="onToggle" />
          </div>
        </div>
        <t-divider style="margin: 4px 0 24px" />

        <div class="field">
          <label class="lbl">名称</label>
          <t-input v-model="cur.name" placeholder="输入名称" />
        </div>
        <div class="field">
          <label class="lbl">API Key</label>
          <t-input v-model="cur.apiKey" type="password" placeholder="输入 API Key" />
        </div>
        <div class="grid2">
          <div class="field">
            <label class="lbl">Search Type</label>
            <t-input v-model="cur.searchType" placeholder="SEARCH_TYPE_XXX" />
          </div>
          <div class="field">
            <label class="lbl">Timeout (seconds)</label>
            <t-input-number v-model="cur.timeout" :min="1" :max="120" style="width: 100%" />
          </div>
        </div>
        <div class="field">
          <label class="lbl">Base URL</label>
          <t-input v-model="cur.baseUrl" placeholder="https://..." />
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
      :confirm-btn="{ content: '确认', disabled: !addForm.name || !addForm.type }"
      :on-confirm="confirmAdd"
      dialogClassName="sp-add-dialog"
    >
      <div class="add-form">
        <div class="field">
          <label class="lbl">名称</label>
          <t-input v-model="addForm.name" placeholder="输入名称" @enter="confirmAdd" />
        </div>
        <div class="field">
          <label class="lbl">提供方类型</label>
          <t-select v-model="addForm.type" :options="typeOptions" placeholder="选择类型" />
        </div>
      </div>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, computed } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { searchProviderApi } from '@/api/services'

const providers = ref([])
const cur = ref(null)
const saving = ref(false)

const typeOptions = computed(() =>
  searchProviderApi.types().map(t => ({ label: t.label, value: t.type }))
)

async function load(selectId) {
  const res = await searchProviderApi.list()
  providers.value = res.providers || []
  if (selectId) cur.value = providers.value.find(p => p.id === selectId) || null
  else if (cur.value) cur.value = providers.value.find(p => p.id === cur.value.id) || null
  else cur.value = providers.value[0] || null
}
function select(p) { cur.value = p }

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

async function save() {
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
const addForm = reactive({ name: '', type: '' })
function openAdd() { addForm.name = ''; addForm.type = ''; addVisible.value = true }
async function confirmAdd() {
  if (!addForm.name.trim() || !addForm.type) return MessagePlugin.warning('请填写名称并选择类型')
  const created = await searchProviderApi.create({ name: addForm.name.trim(), type: addForm.type })
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
  border-right: 1px solid #f0f0f0; background: #fafafa;
}
.side-list { flex: 1; overflow-y: auto; padding: 12px; display: flex; flex-direction: column; gap: 4px; }
.sp-item {
  display: flex; align-items: center; gap: 10px; padding: 9px 12px;
  border-radius: 10px; cursor: pointer;
}
.sp-item:hover { background: #f0f0f0; }
.sp-item.active { background: #fff; box-shadow: 0 1px 3px rgba(0,0,0,0.06); }
.sp-icon {
  width: 24px; height: 24px; border-radius: 6px; flex-shrink: 0; color: #fff;
  display: flex; align-items: center; justify-content: center; font-size: 13px; font-weight: 700;
}
.sp-icon.lg { width: 32px; height: 32px; border-radius: 8px; font-size: 16px; }
.sp-name { flex: 1; min-width: 0; font-size: 14px; color: #1d1d1f; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sp-on { width: 8px; height: 8px; border-radius: 50%; background: #00c853; flex-shrink: 0; }
.side-foot { padding: 12px; border-top: 1px solid #f0f0f0; }

/* 右栏 */
.sp-main { flex: 1; min-width: 0; overflow-y: auto; padding: 24px 32px; }
.sp-empty {
  height: 100%; min-height: 420px; display: flex; flex-direction: column;
  align-items: center; justify-content: center; gap: 10px; color: #aaa;
}
.e-desc { font-size: 13px; }

.detail-head { display: flex; align-items: center; justify-content: space-between; }
.dh-left { display: flex; align-items: center; gap: 12px; }
.dh-name { font-size: 20px; font-weight: 700; color: #1d1d1f; }
.dh-right { display: flex; align-items: center; gap: 10px; }
.en-label { font-size: 13px; color: #888; }

.field { display: flex; flex-direction: column; margin-bottom: 18px; }
.lbl { font-size: 13px; font-weight: 600; color: #1d1d1f; margin-bottom: 8px; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; }

.detail-foot { display: flex; justify-content: flex-end; align-items: center; gap: 12px; margin-top: 8px; }

.add-form .field { margin-bottom: 18px; }
</style>

<style>
.sp-add-dialog.t-dialog { padding: 20px 20px 16px; }
.sp-add-dialog .t-dialog__header { padding: 0; margin-bottom: 16px; }
.sp-add-dialog .t-dialog__body { padding: 0; }
.sp-add-dialog .t-dialog__footer { padding: 16px 0 0; }
</style>
