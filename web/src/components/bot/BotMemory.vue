<template>
  <div>
    <div class="toolbar">
      <t-space>
        <span class="hint">该 Bot 的分层记忆（L0 工作 / L1 长期 / L2 场景 / L3 画像），实时存于数据库。</span>
        <t-select v-model="tier" :options="tierOptions" style="width: 140px" @change="load" data-testid="memory-tier" />
      </t-space>
      <t-space>
        <t-tag variant="light" theme="primary">L1: {{ stats.l1Count ?? 0 }}</t-tag>
        <t-tag variant="light">L2(估): {{ stats.l2Estimate ?? 0 }}</t-tag>
        <t-button size="small" variant="outline" @click="load" data-testid="memory-refresh">刷新</t-button>
      </t-space>
    </div>

    <t-alert v-if="dreamingOff" theme="info" class="guide">
      该 Bot 尚未启用「梦境巩固」，分层记忆存储不存在。启用后这里会展示并支持管理其分层记忆。
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

const loading = ref(false)
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

function remove(row) {
  const dlg = DialogPlugin.confirm({
    header: '删除记忆',
    body: `确认删除这条「${row.tier}」记忆？该操作不可恢复，且只删这一条。`,
    theme: 'warning',
    onConfirm: async () => {
      try {
        await memoryApi.remove(props.botId, row.id, row.tier, row.scope)
        dlg.destroy()
        MessagePlugin.success('已删除')
        await load()
      } catch (e) {
        MessagePlugin.error('删除失败：' + (e.message || '请稍后重试'))
      }
    }
  })
}

onMounted(load)
</script>

<style scoped>
.toolbar { display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px; flex-wrap: wrap; gap: 12px; }
.hint { color: #888; font-size: 13px; }
.guide { margin-bottom: 16px; }
</style>
