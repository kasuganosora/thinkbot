<template>
  <div>
    <div class="toolbar">
      <t-space>
        <span class="hint">查询该 Bot 的分层记忆（L0~L3）。</span>
        <t-select v-model="tier" :options="tierOptions" style="width: 140px" @change="load" data-testid="memory-tier" />
      </t-space>
      <t-space>
        <t-tag variant="light" theme="primary">L1: {{ stats.l1Count ?? 0 }}</t-tag>
        <t-tag variant="light">L2(估): {{ stats.l2Estimate ?? 0 }}</t-tag>
        <t-button size="small" variant="outline" @click="load" data-testid="memory-refresh">刷新</t-button>
      </t-space>
    </div>

    <t-table :data="entries" :columns="columns" row-key="id" :loading="loading" data-testid="memory-table">
      <template #tier="{ row }"><t-tag variant="light">{{ row.tier }}</t-tag></template>
      <template #importance="{ row }">{{ (row.importance * 100).toFixed(0) }}%</template>
      <template #createdAt="{ row }">{{ formatTime(row.createdAt) }}</template>
    </t-table>
  </div>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { memoryApi } from '@/api/services'
import { formatTime } from '@/utils/format'

const props = defineProps({ botId: { type: String, required: true } })

const loading = ref(false)
const entries = ref([])
const stats = ref({})
const tier = ref('')

const tierOptions = [
  { label: '全部层级', value: '' },
  { label: 'L0', value: 'L0' },
  { label: 'L1', value: 'L1' },
  { label: 'L2', value: 'L2' },
  { label: 'L3', value: 'L3' }
]

const columns = [
  { colKey: 'content', title: '记忆内容' },
  { colKey: 'tier', title: '层级', width: 80 },
  { colKey: 'category', title: '分类', width: 110 },
  { colKey: 'scope', title: '作用域', width: 130 },
  { colKey: 'source', title: '来源', width: 110 },
  { colKey: 'importance', title: '重要度', width: 90 },
  { colKey: 'createdAt', title: '创建时间', width: 160 }
]

async function load() {
  loading.value = true
  try {
    const [res, st] = await Promise.all([memoryApi.query(props.botId, tier.value, 50), memoryApi.stats(props.botId)])
    entries.value = res.entries || []
    stats.value = st
  } finally {
    loading.value = false
  }
}
onMounted(load)
</script>

<style scoped>
.toolbar { display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px; }
.hint { color: #888; font-size: 13px; }
</style>
