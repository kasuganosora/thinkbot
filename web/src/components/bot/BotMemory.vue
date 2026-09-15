<template>
  <div>
    <div class="toolbar">
      <t-space>
        <span class="hint">该 Bot 的分层记忆（L0 工作 / L1 长期 / L2 场景 / L3 画像）。L3 是梦境蒸馏的用户/Bot 画像，可按条删除。</span>
        <t-select v-model="tier" :options="tierOptions" style="width: 140px" @change="load" data-testid="memory-tier" />
      </t-space>
      <t-space>
        <t-tag variant="light" theme="primary">L1: {{ stats.l1Count ?? 0 }}</t-tag>
        <t-tag variant="light">L2(估): {{ stats.l2Estimate ?? 0 }}</t-tag>
        <t-tag variant="light" theme="warning">L3: {{ stats.l3Count ?? 0 }}</t-tag>
        <input ref="fileInput" type="file" accept=".tar,.gz,.tgz,application/gzip,application/x-tar" class="file-hidden" @change="onImportFile" />
        <t-button size="small" variant="outline" :loading="importing" @click="pickImport" data-testid="memory-import">导入 Memoh 备份</t-button>
        <t-button size="small" variant="outline" @click="load" data-testid="memory-refresh">刷新</t-button>
      </t-space>
    </div>

    <t-alert v-if="dreamingOff" theme="info" class="guide">
      <div class="guide-row">
        <span>该 Bot 尚未启用「梦境巩固」，分层记忆存储不存在。启用后这里会展示并支持管理其分层记忆。</span>
        <t-button size="small" variant="outline" data-testid="memory-open-dreaming" @click="$emit('open-dreaming')">去启用</t-button>
      </div>
    </t-alert>

    <t-table
      v-else
      :data="entries"
      :columns="columns"
      row-key="id"
      :loading="loading"
      data-testid="memory-table"
      :pagination="{ defaultPageSize: 20, total: entries.length }"
    >
      <template #tier="{ row }"><t-tag variant="light">{{ row.tier }}</t-tag></template>
      <template #importance="{ row }">{{ (row.importance * 100).toFixed(0) }}%</template>
      <template #createdAt="{ row }">{{ formatTime(row.createdAt) }}</template>
      <template #op="{ row }">
        <t-link theme="danger" hover="color" @click="remove(row)">删除</t-link>
      </template>
    </t-table>

    <t-empty v-if="!dreamingOff && !loading && entries.length === 0" description="暂无分层记忆" />
  </div>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { memoryApi } from '@/api/services'
import { formatTime } from '@/utils/format'

const props = defineProps({ botId: { type: String, required: true } })
defineEmits(['open-dreaming'])

const loading = ref(false)
const importing = ref(false)
const fileInput = ref(null)
const entries = ref([])
const stats = ref({})
const tier = ref('')
const dreamingOff = ref(false)

const tierOptions = [
  { label: '全部层级', value: '' },
  { label: 'L0', value: 'L0' },
  { label: 'L1', value: 'L1' },
  { label: 'L2', value: 'L2' },
  { label: 'L3', value: 'L3' }
]

const columns = [
  { colKey: 'content', title: '记忆内容', ellipsis: true, minWidth: 240 },
  { colKey: 'tier', title: '层级', width: 80 },
  { colKey: 'category', title: '分类', width: 110 },
  { colKey: 'scope', title: '作用域', width: 150, ellipsis: true },
  { colKey: 'source', title: '来源', width: 110 },
  { colKey: 'importance', title: '重要度', width: 90 },
  { colKey: 'createdAt', title: '创建时间', width: 160 },
  { colKey: 'op', title: '操作', width: 70 }
]

async function load() {
  loading.value = true
  try {
    const [res, st] = await Promise.all([memoryApi.query(props.botId, tier.value, 100), memoryApi.stats(props.botId)])
    dreamingOff.value = res.enabled === false
    entries.value = res.entries || []
    stats.value = st
  } finally {
    loading.value = false
  }
}

function pickImport() {
  fileInput.value?.click()
}

async function onImportFile(ev) {
  const file = ev.target.files && ev.target.files[0]
  ev.target.value = ''
  if (!file) return
  importing.value = true
  try {
    const res = await memoryApi.importMemoh(props.botId, file)
    const junk = res.skippedJunk ?? 0
    const imported = res.imported ?? 0
    MessagePlugin.success(`导入完成：写入 ${imported} 条，跳过废物 ${junk} 条`)
    await load()
  } catch (e) {
    MessagePlugin.error('导入失败：' + (e.message || '请稍后重试'))
  } finally {
    importing.value = false
  }
}

function remove(row) {
  if (!row.scope) {
    // 后端要求 id+tier+scope 三者齐全才定位到唯一一条；缺 scope 会 400。
    // 提前拦住并给出可读原因，避免"弹框一闪而过、看起来没反应"。
    MessagePlugin.error('这条记忆缺少作用域信息（scope），无法定位删除')
    return
  }
  const dlg = DialogPlugin.confirm({
    header: '删除记忆',
    body: `确认删除这条「${row.tier}」记忆？该操作不可恢复，且只删这一条。`,
    theme: 'warning',
    // TDesign 1.20 的命令式 dialog：destroy() 内部只是 visible=false + 300ms 后移除 wrapper，
    // 但 .t-dialog__ctx（含 .t-dialog__mask 遮罩）节点经常回收不掉，会在 DOM 里残留 0×0 的
    // 遮罩/定位容器，累积后可能盖在表格上吞掉点击（踩过一次）。
    // 因此三条关闭路径都先 destroy()，再显式清理已隐藏的游离对话框容器。
    onCancel: () => { dlg.destroy(); removeOrphanDialogCtx() },
    onClose: () => { dlg.destroy(); removeOrphanDialogCtx() },
    onConfirm: async () => {
      try {
        await memoryApi.remove(props.botId, row.id, row.tier, row.scope)
        MessagePlugin.success('已删除')
        await load()
      } catch (e) {
        MessagePlugin.error('删除失败：' + (e.message || '请稍后重试'))
      } finally {
        dlg.destroy()
        removeOrphanDialogCtx()
      }
    }
  })
}

// 关闭命令式 dialog 后，清理仍残留在 DOM 里的 .t-dialog__ctx（含 .t-dialog__mask 遮罩）节点。
// TDesign 1.20 关闭时通过 v-if 把内部 .t-dialog 盒子移除（不会 display:none，遮罩节点因此残留），
// 累积后会盖住表格吞掉点击。这里移除「不含可见 .t-dialog 盒子」的游离 ctx——
// 正在显示的对话框盒子可见（offsetParent 非空）会被保留，关闭后盒子被移除的孤儿才会被清掉。
// 350ms 晚于 TDesign 内部 300ms 的关闭过渡，确保盒子已真正卸载。
function removeOrphanDialogCtx() {
  setTimeout(() => {
    document.querySelectorAll('.t-dialog__ctx').forEach((ctx) => {
      const box = ctx.querySelector('.t-dialog')
      const active = box && box.offsetParent !== null
      if (!active) ctx.remove()
    })
  }, 350)
}

onMounted(load)
</script>

<style scoped>
.toolbar { display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px; flex-wrap: wrap; gap: 12px; }
.hint { color: var(--bp-label-tertiary); font-size: 13px; }
.guide { margin-bottom: 16px; }
.guide-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.file-hidden { display: none; }
</style>
