<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { Eye, EyeOff, Save } from 'lucide-vue-next'
import { configApi, systemApi } from '@/api/client'
import { useToast } from '@/components/ui'
import {
  TButton, TInput,
  TSpinner, TEmpty, TPageHeader, TTabBar, TCard,
} from '@/components/ui'
import type { ConfigItem, SystemHealth } from '@/types/api'

const toast = useToast()
const allConfigs = ref<ConfigItem[]>([])
const loading = ref(true)
const saving = ref(false)
const activeTab = ref('')
const health = ref<SystemHealth | null>(null)

const categories = computed(() => {
  const cats: Record<string, ConfigItem[]> = {}
  for (const c of allConfigs.value) {
    const cat = c.category || 'Other'
    if (!cats[cat]) cats[cat] = []
    cats[cat].push(c)
  }
  const order = ['System', 'API', 'Bot', 'Database', 'Log']
  const sorted = Object.keys(cats).sort((a, b) => {
    const ia = order.indexOf(a)
    const ib = order.indexOf(b)
    if (ia !== -1 || ib !== -1) {
      if (ia === -1) return 1
      if (ib === -1) return -1
      return ia - ib
    }
    return a.localeCompare(b)
  })
  return { cats, sorted }
})

const tabs = computed(() => [
  ...categories.value.sorted.map(c => ({ id: c, label: c })),
  { id: '__system', label: '系统监控' },
])

const editValues = ref<Record<string, string>>({})
const isSecret = (key: string) =>
  key.toLowerCase().includes('key') ||
  key.toLowerCase().includes('secret') ||
  key.toLowerCase().includes('password')
const revealedKeys = ref<Set<string>>(new Set())

function toggleReveal(key: string) {
  if (revealedKeys.value.has(key)) revealedKeys.value.delete(key)
  else revealedKeys.value.add(key)
}

async function load() {
  loading.value = true
  try {
    const items = await configApi.list()
    allConfigs.value = items
    for (const c of items) editValues.value[c.key] = c.value
    if (!activeTab.value && categories.value.sorted.length > 0) {
      activeTab.value = categories.value.sorted[0]
    }
    systemApi.health().then((h) => (health.value = h)).catch(() => {})
  } finally {
    loading.value = false
  }
}

async function save(key: string) {
  saving.value = true
  try {
    await configApi.set(key, editValues.value[key])
    toast.success('已保存')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '保存失败')
  } finally {
    saving.value = false
  }
}

async function saveAll(cat: string) {
  const items = categories.value.cats[cat] || []
  const batch: Record<string, string> = {}
  for (const c of items) batch[c.key] = editValues.value[c.key]
  saving.value = true
  try {
    await configApi.batchSet(batch)
    toast.success('已全部保存')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '保存失败')
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <TPageHeader title="系统设置" subtitle="管理系统配置和监控服务状态" />

    <TTabBar :tabs="tabs" v-model="activeTab" />

    <div v-if="loading" class="loading-state"><TSpinner size="lg" /></div>

    <div v-else class="tab-content">
      <!-- 系统监控 -->
      <div v-if="activeTab === '__system'" class="page-content">
        <div v-if="health" class="monitor-grid">
          <TCard padding="default">
            <p class="stat-label">状态</p>
            <p class="stat-value" :class="{ 'text-success': health.status === 'ok' }">{{ health.status }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">主机名</p>
            <p class="stat-value-sm">{{ health.host?.hostname || '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">运行时间</p>
            <p class="stat-value-sm">{{ health.uptime || '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">Go 版本</p>
            <p class="stat-value-sm">{{ health.goVersion || '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">Goroutines</p>
            <p class="stat-value">{{ health.goroutines ?? '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">CPU 核数</p>
            <p class="stat-value">{{ health.host?.cpuCount ?? '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">内存分配</p>
            <p class="stat-value-sm">{{ health.memory?.allocMB?.toFixed(1) ?? '-' }} MB</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">GC 次数</p>
            <p class="stat-value">{{ health.memory?.numGC ?? '-' }}</p>
          </TCard>
          <TCard padding="default">
            <p class="stat-label">运行中 Bot</p>
            <p class="stat-value">{{ health.bots?.running ?? '-' }}</p>
          </TCard>
        </div>
      </div>

      <!-- 配置列表 -->
      <div v-else class="page-content-narrow">
        <div v-for="c in (categories.cats[activeTab] || [])" :key="c.key" class="config-row">
          <div class="config-info">
            <p class="config-label">{{ c.description || c.key }}</p>
            <p class="config-key">{{ c.key }}</p>
          </div>
          <div class="config-input">
            <TInput
              v-model="editValues[c.key]"
              :type="isSecret(c.key) && !revealedKeys.has(c.key) ? 'password' : 'text'"
            />
            <TButton
              v-if="isSecret(c.key)"
              variant="ghost"
              size="sm"
              @click="toggleReveal(c.key)"
            >
              <component :is="revealedKeys.has(c.key) ? EyeOff : Eye" :size="14" />
            </TButton>
            <TButton
              variant="ghost"
              size="sm"
              :disabled="saving"
              @click="save(c.key)"
            >
              <Save :size="14" />
            </TButton>
          </div>
        </div>
        <div v-if="(categories.cats[activeTab] || []).length === 0" class="empty-inline">该分类下暂无配置项</div>
        <div
          v-if="(categories.cats[activeTab] || []).length > 0"
          class="form-actions"
        >
          <TButton :loading="saving" @click="saveAll(activeTab)">全部保存</TButton>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
@import '@/assets/page-common.css';

.monitor-grid {
  display: grid;
  gap: 0.75rem;
  grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
}

.config-row {
  display: flex;
  align-items: flex-start;
  gap: 1rem;
  padding: 0.75rem 0;
  border-bottom: 1px solid var(--border);
}
.config-row:last-of-type { border-bottom: none; }

.config-info {
  flex-shrink: 0;
  width: 280px;
}
.config-label {
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--foreground);
  line-height: 1.4;
}
.config-key {
  font-size: 0.6875rem;
  font-family: var(--font-mono);
  color: var(--muted-foreground);
  margin-top: 0.125rem;
}

.config-input {
  flex: 1;
  display: flex;
  gap: 0.25rem;
  align-items: center;
}
</style>
