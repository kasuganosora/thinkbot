<script setup lang="ts">
import { ref, onMounted, watch, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ArrowLeft, Play, Square } from 'lucide-vue-next'
import { botsApi, statsApi, llmApi } from '@/api/client'
import { useToast } from '@/components/ui'
import {
  TButton, TInput, TTextarea, TSelect, TCheckbox,
  TBadge, TSpinner, TPageHeader, TTabBar, TCard,
} from '@/components/ui'
import type { BotDefinition, BotStats, BotDailyStats, LLMModel } from '@/types/api'

const route = useRoute()
const router = useRouter()
const toast = useToast()
const botId = computed(() => route.params.id as string)

const bot = ref<(BotDefinition & { running: boolean }) | null>(null)
const loading = ref(true)
const saving = ref(false)
const activeTab = ref('overview')
const stats = ref<BotStats | null>(null)
const dailyStats = ref<BotDailyStats | null>(null)
const cronJobs = ref<{ jobs: unknown[]; total: number } | null>(null)
const dreamingCfg = ref<{ enabled: boolean; schedule: string } | null>(null)
const llmModels = ref<LLMModel[]>([])

const tabs = [
  { id: 'overview', label: '概览' },
  { id: 'config', label: '配置' },
  { id: 'stats', label: '消耗统计' },
  { id: 'cron', label: '定时任务' },
  { id: 'dreaming', label: '梦境巩固' },
]

const editForm = ref<Partial<BotDefinition>>({})

async function load() {
  loading.value = true
  try {
    bot.value = await botsApi.get(botId.value)
    editForm.value = { ...bot.value }
    llmApi.list().then((m) => (llmModels.value = m)).catch(() => {})
    statsApi.bot(botId.value).then((s) => (stats.value = s)).catch(() => {})
    statsApi.botDaily(botId.value).then((s) => (dailyStats.value = s)).catch(() => {})
    botsApi.listCronJobs(botId.value).then((c) => (cronJobs.value = c)).catch(() => {})
    botsApi.getDreaming(botId.value).then((d) => (dreamingCfg.value = d)).catch(() => {})
  } finally {
    loading.value = false
  }
}

async function saveConfig() {
  saving.value = true
  try {
    await botsApi.update(botId.value, editForm.value)
    bot.value = await botsApi.get(botId.value)
    toast.success('配置已保存')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '保存失败')
  } finally {
    saving.value = false
  }
}

async function toggleStart() {
  if (!bot.value) return
  try {
    if (bot.value.running) await botsApi.stop(botId.value)
    else await botsApi.start(botId.value)
    bot.value = await botsApi.get(botId.value)
    toast.success(bot.value.running ? '已启动' : '已停止')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '操作失败')
  }
}

watch(botId, load)
onMounted(load)
</script>

<template>
  <div class="page">
    <TPageHeader :title="bot?.name || '加载中…'" :subtitle="bot ? `${bot.id}` : ''">
      <template #back>
        <TButton variant="ghost" size="icon" @click="router.push('/settings/bots')">
          <ArrowLeft :size="18" />
        </TButton>
      </template>
      <template #actions>
        <TBadge v-if="bot" :variant="bot.running ? 'success' : 'secondary'">
          {{ bot.running ? '运行中' : '已停止' }}
        </TBadge>
        <TButton v-if="bot" :variant="bot.running ? 'outline' : 'default'" @click="toggleStart">
          <component :is="bot.running ? Square : Play" :size="14" />
          {{ bot.running ? '停止' : '启动' }}
        </TButton>
      </template>
    </TPageHeader>

    <TTabBar :tabs="tabs" v-model="activeTab" />

    <div v-if="loading" class="loading-state"><TSpinner size="lg" /></div>

    <div v-else class="tab-content">
      <!-- 概览 -->
      <div v-if="activeTab === 'overview'" class="page-content">
        <div class="overview-grid">
          <TCard padding="default">
            <p class="stat-label">状态</p>
            <p class="stat-value" :class="{ 'text-success': bot?.running }">
              {{ bot?.running ? '运行中' : '已停止' }}
            </p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">主 LLM</p>
            <p class="stat-value-sm">{{ bot?.llmMain || '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">轻量 LLM</p>
            <p class="stat-value-sm">{{ bot?.llmLight || '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">温度</p>
            <p class="stat-value-sm">{{ bot?.temperature ?? '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">最大 Token</p>
            <p class="stat-value-sm">{{ bot?.maxTokens || '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">深度思考</p>
            <p class="stat-value-sm">{{ bot?.reasoningEffort || '禁用' }}</p>
          </TCard>
        </div>
      </div>

      <!-- 配置 -->
      <div v-if="activeTab === 'config'" class="page-content-narrow">
        <TCard padding="lg">
          <div class="form-field">
            <label>名称</label>
            <TInput v-model="editForm.name" />
          </div>
          <div class="form-field">
            <label>系统提示词</label>
            <TTextarea v-model="editForm.systemPrompt" :rows="6" />
          </div>
          <div class="form-grid">
            <div class="form-field">
              <label>主 LLM</label>
              <TSelect v-if="llmModels.length > 0" v-model="editForm.llmMain">
                <option value="">不指定</option>
                <option v-for="m in llmModels" :key="m.id" :value="m.id">
                  {{ m.id }} ({{ m.provider }} {{ m.model }})
                </option>
              </TSelect>
              <TInput v-else v-model="editForm.llmMain" placeholder="先在模型管理中配置" />
            </div>
            <div class="form-field">
              <label>轻量 LLM</label>
              <TSelect v-if="llmModels.length > 0" v-model="editForm.llmLight">
                <option value="">不指定</option>
                <option v-for="m in llmModels" :key="m.id" :value="m.id">
                  {{ m.id }} ({{ m.provider }} {{ m.model }})
                </option>
              </TSelect>
              <TInput v-else v-model="editForm.llmLight" placeholder="可选" />
            </div>
          </div>
          <div class="form-grid">
            <div class="form-field">
              <label>温度</label>
              <TInput v-model="editForm.temperature" type="number" />
            </div>
            <div class="form-field">
              <label>深度思考</label>
              <TSelect v-model="editForm.reasoningEffort">
                <option value="">禁用</option>
                <option value="minimal">极简 (Minimal)</option>
                <option value="low">低 (Low)</option>
                <option value="medium">中 (Medium)</option>
                <option value="high">高 (High)</option>
              </TSelect>
            </div>
          </div>
          <div class="form-grid">
            <div class="form-field">
              <label>最大 Token</label>
              <TInput v-model="editForm.maxTokens" type="number" />
            </div>
            <div class="form-field">
              <label>Worker 数</label>
              <TInput v-model="editForm.workers" type="number" />
            </div>
          </div>
          <div class="form-actions">
            <TButton :loading="saving" @click="saveConfig">保存</TButton>
          </div>
        </TCard>
      </div>

      <!-- 消耗统计 -->
      <div v-if="activeTab === 'stats'" class="page-content">
        <div class="overview-grid">
          <TCard padding="default">
            <p class="stat-label">总请求数</p>
            <p class="stat-value">{{ stats?.totalRequests ?? '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">输入 Token</p>
            <p class="stat-value">{{ stats?.totalInputTokens?.toLocaleString() ?? '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">输出 Token</p>
            <p class="stat-value">{{ stats?.totalOutputTokens?.toLocaleString() ?? '-' }}</p>
          </TCard>
        </div>
        <div v-if="stats?.byModel" class="section">
          <h3 class="section-title">按模型</h3>
          <div class="table-wrap">
            <table class="data-table">
              <thead><tr><th>模型</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th></tr></thead>
              <tbody>
                <tr v-for="(v, k) in stats.byModel" :key="k">
                  <td class="mono">{{ k }}</td><td>{{ v.requests }}</td><td>{{ v.inputTokens?.toLocaleString() }}</td><td>{{ v.outputTokens?.toLocaleString() }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
        <div v-if="dailyStats?.daily?.length" class="section">
          <h3 class="section-title">每日趋势</h3>
          <div class="table-wrap">
            <table class="data-table">
              <thead><tr><th>日期</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th></tr></thead>
              <tbody>
                <tr v-for="d in dailyStats.daily" :key="d.date">
                  <td class="mono">{{ d.date }}</td><td>{{ d.requests }}</td><td>{{ d.inputTokens?.toLocaleString() }}</td><td>{{ d.outputTokens?.toLocaleString() }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      <!-- 定时任务 -->
      <div v-if="activeTab === 'cron'" class="page-content">
        <div v-if="!cronJobs || cronJobs.total === 0" class="empty-inline">暂无定时任务</div>
        <div v-else class="table-wrap">
          <table class="data-table">
            <thead><tr><th>名称</th><th>计划</th><th>状态</th><th>运行次数</th></tr></thead>
            <tbody>
              <tr v-for="(job, i) in (cronJobs.jobs as any[])" :key="i">
                <td>{{ job.name }}</td><td class="mono">{{ job.schedule }}</td><td>{{ job.enabled ? '启用' : '暂停' }}</td><td>{{ job.runCount || 0 }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- 梦境巩固 -->
      <div v-if="activeTab === 'dreaming'" class="page-content-narrow">
        <TCard padding="lg">
          <div class="form-field">
            <TCheckbox v-model="dreamingCfg!.enabled">启用梦境巩固</TCheckbox>
          </div>
          <div class="form-field">
            <label>定时计划 (Cron)</label>
            <TInput v-model="dreamingCfg!.schedule" placeholder="0 3 * * *" />
          </div>
          <div class="form-actions">
            <TButton @click="botsApi.updateDreaming(botId, dreamingCfg!)">保存</TButton>
          </div>
        </TCard>
      </div>
    </div>
  </div>
</template>

<style scoped>
@import '@/assets/page-common.css';

.overview-grid {
  display: grid;
  gap: 0.75rem;
  grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
}

.section { margin-bottom: 1.5rem; }
</style>
