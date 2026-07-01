<template>
  <SettingsShell title="统计概览">
    <div class="summary-cards" data-testid="stats-summary">
      <t-card :bordered="false" class="metric-card">
        <div class="metric-label">总请求数</div>
        <div class="metric-value" data-testid="stats-total-requests">{{ summary.totalRequests }}</div>
      </t-card>
      <t-card :bordered="false" class="metric-card">
        <div class="metric-label">总 Token 数</div>
        <div class="metric-value" data-testid="stats-total-tokens">{{ summary.totalTokens }}</div>
      </t-card>
      <t-card :bordered="false" class="metric-card">
        <div class="metric-label">总工具调用</div>
        <div class="metric-value" data-testid="stats-total-toolcalls">{{ summary.toolCalls }}</div>
      </t-card>
    </div>

    <t-card title="各 Bot 用量统计" :bordered="false" class="card">
      <t-table
        row-key="botId"
        data-testid="stats-overview-table"
        :data="overview"
        :columns="overviewColumns"
        :loading="loading"
        size="small"
        hover
      >
        <template #op="{ row }">
          <t-button
            variant="text"
            theme="primary"
            size="small"
            :data-testid="`stats-daily-btn-${row.botId}`"
            @click="loadDaily(row.botId)"
          >查看趋势</t-button>
        </template>
      </t-table>
    </t-card>

    <t-card v-if="selectedBot" :title="`每日趋势 · ${selectedBot}`" :bordered="false" class="card">
      <t-table
        row-key="date"
        data-testid="stats-daily-table"
        :data="daily"
        :columns="dailyColumns"
        :loading="dailyLoading"
        size="small"
        hover
      >
        <template #date="{ row }">{{ formatTime(row.date) }}</template>
      </t-table>
    </t-card>
  </SettingsShell>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import SettingsShell from '@/components/SettingsShell.vue'
import { statsApi } from '@/api/services'

const loading = ref(false)
const dailyLoading = ref(false)
const overview = ref([])
const daily = ref([])
const selectedBot = ref('')

const overviewColumns = [
  { colKey: 'botId', title: 'Bot ID', width: 140 },
  { colKey: 'model', title: '模型', width: 120 },
  { colKey: 'totalRequests', title: '总请求', width: 90 },
  { colKey: 'cacheHitRequests', title: '缓存命中', width: 90 },
  { colKey: 'cacheMissRequests', title: '缓存未命中', width: 100 },
  { colKey: 'inputTokens', title: '输入 Token', width: 110 },
  { colKey: 'outputTokens', title: '输出 Token', width: 110 },
  { colKey: 'totalTokens', title: '总 Token', width: 100 },
  { colKey: 'toolCalls', title: '工具调用', width: 90 },
  { colKey: 'op', title: '操作', width: 100, fixed: 'right' }
]

const dailyColumns = [
  { colKey: 'date', title: '日期', width: 180 },
  { colKey: 'totalRequests', title: '总请求' },
  { colKey: 'cacheHitRequests', title: '缓存命中' },
  { colKey: 'cacheMissRequests', title: '缓存未命中' },
  { colKey: 'totalTokens', title: '总 Token' }
]

const summary = computed(() => {
  return overview.value.reduce(
    (acc, r) => {
      acc.totalRequests += r.totalRequests || 0
      acc.totalTokens += r.totalTokens || 0
      acc.toolCalls += r.toolCalls || 0
      return acc
    },
    { totalRequests: 0, totalTokens: 0, toolCalls: 0 }
  )
})

function formatTime(iso) {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return String(iso)
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

async function loadOverview() {
  loading.value = true
  try {
    overview.value = await statsApi.overview()
  } catch (e) {
    MessagePlugin.error(`加载统计概览失败：${e.message || e}`)
  } finally {
    loading.value = false
  }
}

async function loadDaily(botId) {
  selectedBot.value = botId
  dailyLoading.value = true
  try {
    daily.value = await statsApi.daily(botId)
  } catch (e) {
    MessagePlugin.error(`加载每日趋势失败：${e.message || e}`)
  } finally {
    dailyLoading.value = false
  }
}

onMounted(loadOverview)
</script>

<style scoped>
.summary-cards {
  display: flex;
  gap: 16px;
  margin-bottom: 20px;
}
.metric-card {
  flex: 1;
  text-align: center;
}
.metric-label {
  font-size: 13px;
  color: #8a8a8a;
  margin-bottom: 8px;
}
.metric-value {
  font-size: 26px;
  font-weight: 600;
}
.card {
  margin-bottom: 20px;
}
</style>
