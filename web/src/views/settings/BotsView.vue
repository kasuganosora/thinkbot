<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { Plus, Search, X } from 'lucide-vue-next'
import { botsApi, llmApi } from '@/api/client'
import { useToast } from '@/components/ui'
import {
  TButton, TInput, TTextarea, TSelect,
  TBadge, TSpinner, TEmpty, TPageHeader, TCard,
} from '@/components/ui'
import type { BotDefinition, LLMModel } from '@/types/api'

const router = useRouter()
const toast = useToast()
const bots = ref<(BotDefinition & { running: boolean })[]>([])
const llmModels = ref<LLMModel[]>([])
const loading = ref(true)
const search = ref('')
const showCreate = ref(false)
const creating = ref(false)

const newForm = ref({
  id: '',
  name: '',
  systemPrompt: '',
  llmMain: '',
  llmLight: '',
  temperature: 0.7,
  maxTokens: 4096,
  workers: 2,
})

function resetForm() {
  newForm.value = {
    id: '', name: '', systemPrompt: '', llmMain: '', llmLight: '',
    temperature: 0.7, maxTokens: 4096, workers: 2,
  }
}

const filtered = () =>
  bots.value.filter((b) =>
    b.name.toLowerCase().includes(search.value.toLowerCase()) ||
    b.id.toLowerCase().includes(search.value.toLowerCase()),
  )

async function toggleStart(id: string, running: boolean) {
  try {
    if (running) await botsApi.stop(id)
    else await botsApi.start(id)
    await load()
    toast.success(running ? '已停止' : '已启动')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '操作失败')
  }
}

async function create() {
  if (!newForm.value.id.trim()) {
    toast.warning('请输入 Bot ID')
    return
  }
  if (!newForm.value.name.trim()) {
    toast.warning('请输入机器人名称')
    return
  }
  creating.value = true
  try {
    await botsApi.create(newForm.value)
    const createdId = newForm.value.id
    showCreate.value = false
    resetForm()
    await load()
    router.push(`/settings/bots/${createdId}`)
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '创建失败')
  } finally {
    creating.value = false
  }
}

async function load() {
  loading.value = true
  try {
    const [botList, modelList] = await Promise.all([
      botsApi.list(),
      llmApi.list().catch(() => [] as LLMModel[]),
    ])
    bots.value = botList
    llmModels.value = modelList
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <TPageHeader title="机器人" subtitle="管理和配置你的 AI 机器人">
      <template #actions>
        <div class="search-wrap">
          <Search :size="14" class="search-icon" />
          <input v-model="search" class="search-box" placeholder="搜索…" />
        </div>
        <TButton @click="showCreate = !showCreate">
          <Plus :size="14" />
          创建机器人
        </TButton>
      </template>
    </TPageHeader>

    <!-- Inline create panel -->
    <div v-if="showCreate" class="page-content-narrow create-panel">
      <TCard padding="lg">
        <div class="create-panel-header">
          <div>
            <h2 class="create-panel-title">创建新机器人</h2>
            <p class="create-panel-subtitle">填写基本信息，创建后可进入详情页进行详细配置</p>
          </div>
          <TButton variant="ghost" size="icon" @click="showCreate = false">
            <X :size="16" />
          </TButton>
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>Bot ID</label>
            <TInput v-model="newForm.id" placeholder="英文标识符，如 alice" />
          </div>
          <div class="form-field">
            <label>名称</label>
            <TInput v-model="newForm.name" placeholder="显示名称" />
          </div>
        </div>
        <div class="form-field">
          <label>系统提示词</label>
          <TTextarea v-model="newForm.systemPrompt" :rows="4" placeholder="定义机器人的性格和行为" />
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>主 LLM</label>
            <TSelect v-if="llmModels.length > 0" v-model="newForm.llmMain">
              <option value="">不指定</option>
              <option v-for="m in llmModels" :key="m.id" :value="m.id">
                {{ m.id }} ({{ m.provider }} {{ m.model }})
              </option>
            </TSelect>
            <TInput v-else v-model="newForm.llmMain" placeholder="先在模型管理中配置" />
          </div>
          <div class="form-field">
            <label>轻量 LLM</label>
            <TSelect v-if="llmModels.length > 0" v-model="newForm.llmLight">
              <option value="">不指定</option>
              <option v-for="m in llmModels" :key="m.id" :value="m.id">
                {{ m.id }} ({{ m.provider }} {{ m.model }})
              </option>
            </TSelect>
            <TInput v-else v-model="newForm.llmLight" placeholder="可选" />
          </div>
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>Worker 数</label>
            <TInput v-model="newForm.workers" type="number" />
          </div>
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>温度</label>
            <TInput v-model="newForm.temperature" type="number" />
          </div>
          <div class="form-field">
            <label>最大 Token</label>
            <TInput v-model="newForm.maxTokens" type="number" />
          </div>
        </div>
        <div class="form-actions">
          <TButton variant="ghost" @click="showCreate = false">取消</TButton>
          <TButton :loading="creating" @click="create">创建</TButton>
        </div>
      </TCard>
    </div>

    <div v-if="loading" class="loading-state"><TSpinner size="lg" /></div>

    <TEmpty v-else-if="filtered().length === 0 && !showCreate" text="暂无机器人，点击「创建机器人」开始" />

    <div v-else-if="filtered().length > 0" class="page-content">
      <div class="bot-grid">
        <TCard
          v-for="bot in filtered()"
          :key="bot.id"
          padding="default"
          class="bot-card-wrap"
          @click="router.push(`/settings/bots/${bot.id}`)"
        >
          <div class="bot-card-top">
            <div class="bot-avatar">{{ bot.name.charAt(0).toUpperCase() }}</div>
            <TBadge :variant="bot.running ? 'success' : 'secondary'">
              {{ bot.running ? '运行中' : '已停止' }}
            </TBadge>
          </div>
          <div class="bot-card-body">
            <h3 class="bot-name">{{ bot.name }}</h3>
            <p class="bot-id">{{ bot.id }}</p>
            <p class="bot-meta">{{ bot.llmMain || '未配置 LLM' }}</p>
          </div>
          <TButton
            variant="ghost"
            size="sm"
            @click.stop="toggleStart(bot.id, bot.running)"
          >
            {{ bot.running ? '停止' : '启动' }}
          </TButton>
        </TCard>
      </div>
    </div>
  </div>
</template>

<style scoped>
@import '@/assets/page-common.css';

.bot-grid {
  display: grid;
  gap: 0.75rem;
  grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
}

.bot-card-wrap {
  cursor: pointer;
  transition: border-color 0.15s ease;
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}
.bot-card-wrap:hover {
  border-color: var(--muted-foreground);
}

.bot-card-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.bot-avatar {
  width: 36px;
  height: 36px;
  border-radius: var(--radius-md);
  background: var(--accent);
  color: var(--foreground);
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 0.9375rem;
  font-weight: 600;
}

.bot-card-body {
  flex: 1;
}

.bot-name {
  font-size: 0.9375rem;
  font-weight: 600;
  margin-bottom: 0.125rem;
}

.bot-id {
  font-size: 0.6875rem;
  color: var(--muted-foreground);
  font-family: var(--font-mono);
  margin-bottom: 0.25rem;
}

.bot-meta {
  font-size: 0.75rem;
  color: var(--muted-foreground);
}
</style>
