<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { RefreshCw } from 'lucide-vue-next'
import { statsApi } from '@/api/client'
import { TButton, TSpinner, TEmpty, TPageHeader, TCard } from '@/components/ui'
import type { StatsOverview } from '@/types/api'

const overview = ref<StatsOverview | null>(null)
const loading = ref(true)

async function load() {
  loading.value = true
  try {
    overview.value = await statsApi.overview()
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <TPageHeader title="统计分析" subtitle="全局使用数据概览">
      <template #actions>
        <TButton variant="outline" size="sm" :loading="loading" @click="load">
          <RefreshCw :size="14" />
          刷新
        </TButton>
      </template>
    </TPageHeader>

    <div v-if="loading" class="loading-state"><TSpinner size="lg" /></div>

    <div v-else-if="overview" class="page-content">
      <div class="overview-grid">
        <TCard padding="default">
          <p class="stat-label">总请求数</p>
          <p class="stat-value">{{ overview.totalRequests ?? '-' }}</p>
        </TCard>
        <TCard padding="default">
          <p class="stat-label">输入 Token</p>
          <p class="stat-value">{{ overview.totalInputTokens?.toLocaleString() ?? '-' }}</p>
        </TCard>
        <TCard padding="default">
          <p class="stat-label">输出 Token</p>
          <p class="stat-value">{{ overview.totalOutputTokens?.toLocaleString() ?? '-' }}</p>
        </TCard>
      </div>

      <div v-if="overview.byModel" class="section">
        <h3 class="section-title">按模型分布</h3>
        <div class="table-wrap">
          <table class="data-table">
            <thead><tr><th>模型</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th></tr></thead>
            <tbody>
              <tr v-for="(v, k) in overview.byModel" :key="k">
                <td class="mono">{{ k }}</td><td>{{ v.requests }}</td>
                <td>{{ v.inputTokens?.toLocaleString() }}</td><td>{{ v.outputTokens?.toLocaleString() }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div v-if="overview.bots" class="section">
        <h3 class="section-title">按 Bot 分布</h3>
        <div class="table-wrap">
          <table class="data-table">
            <thead><tr><th>Bot</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th></tr></thead>
            <tbody>
              <tr v-for="b in overview.bots" :key="b.botId">
                <td>{{ b.botName || b.botId }}</td><td>{{ b.requests }}</td>
                <td>{{ b.inputTokens?.toLocaleString() }}</td><td>{{ b.outputTokens?.toLocaleString() }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <TEmpty v-if="!overview.byModel && !overview.bots" text="暂无统计数据" />
    </div>
  </div>
</template>

<style scoped>
@import '@/assets/page-common.css';

.overview-grid {
  display: grid;
  gap: 0.75rem;
  grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
  margin-bottom: 1.5rem;
}

.section { margin-bottom: 1.5rem; }
</style>
