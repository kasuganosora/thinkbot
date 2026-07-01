<template>
  <div class="bot-shell">
    <!-- 顶部返回栏 -->
    <header class="bot-topbar">
      <button class="back-btn" data-testid="bot-back" @click="goBack">
        <t-icon name="chevron-left" /><span>设置</span>
      </button>
    </header>

    <template v-if="bot">
      <!-- Bot 头部 -->
      <div class="bot-head">
        <div class="bh-avatar">
          <img v-if="bot.avatarUrl" :src="bot.avatarUrl" :alt="bot.name" />
          <span v-else>{{ avatarText }}</span>
        </div>
        <div class="bh-info">
          <div class="bh-name-row">
            <span class="bh-name">{{ bot.name }}</span>
            <t-icon name="edit" class="bh-edit" @click="renameBot" />
          </div>
          <span class="bh-status" :class="{ running: bot.running }">{{ bot.running ? '运行中' : '已停止' }}</span>
        </div>
      </div>

      <!-- 两栏：左子导航 + 右内容 -->
      <div class="bot-body">
        <nav class="bot-nav" aria-label="Bot 设置导航">
          <button
            v-for="it in navItems"
            :key="it.key"
            class="bn-item"
            :class="{ active: activeKey === it.key }"
            :data-testid="`bot-nav-${it.key}`"
            @click="activeKey = it.key"
          >{{ it.label }}</button>
        </nav>

        <section class="bot-content" data-testid="bot-content">
          <!-- 概览 -->
          <BotOverview v-if="activeKey === 'overview'" :bot="bot" />
          <!-- 通用（基础与模型） -->
          <div v-else-if="activeKey === 'general'" class="pad">
            <BotBasicForm :form="form" :emojis="emojis" :model-options="modelOptions" :running="!!bot.running" :toggling="toggling" @save="saveBasic" @remove="removeBot" @toggle-status="toggleStatus" />
          </div>
          <!-- 容器 -->
          <div v-else-if="activeKey === 'container'" class="pad"><BotContainer :bot-id="bot.id" /></div>
          <!-- 记忆 -->
          <BotMemoryFiles v-else-if="activeKey === 'memory'" :bot-id="bot.id" />
          <!-- 平台 -->
          <BotPlatforms v-else-if="activeKey === 'platform'" :bot-id="bot.id" />
          <!-- 访问控制 -->
          <BotAccess v-else-if="activeKey === 'access'" :bot-id="bot.id" />
          <!-- 邮件（占位） -->
          <Placeholder v-else-if="activeKey === 'email'" title="邮件" desc="配置邮件收发渠道（待完善）。" />
          <!-- 终端 -->
          <div v-else-if="activeKey === 'terminal'" class="pad"><BotTerminal :bot-id="bot.id" /></div>
          <!-- 文件 -->
          <BotFiles v-else-if="activeKey === 'files'" :bot-id="bot.id" />
          <!-- MCP -->
          <div v-else-if="activeKey === 'mcp'" class="pad"><BotMcp :bot-id="bot.id" /></div>
          <!-- 心跳 -->
          <div v-else-if="activeKey === 'heartbeat'" class="pad"><BotHeartbeat :bot-id="bot.id" /></div>
          <!-- 上下文压缩 -->
          <div v-else-if="activeKey === 'compact'" class="pad"><BotCompaction :bot-id="bot.id" /></div>
          <!-- 聊天节奏 -->
          <BotRhythm v-else-if="activeKey === 'rhythm'" :bot-id="bot.id" />
          <!-- 人格（占位） -->
          <Placeholder v-else-if="activeKey === 'persona'" title="人格" desc="编辑 Bot 人格与系统提示（待完善）。" />
          <!-- 定时任务 -->
          <div v-else-if="activeKey === 'cron'" class="pad"><BotCronJobs :bot-id="bot.id" /></div>
          <!-- 技能 -->
          <div v-else-if="activeKey === 'skills'" class="pad"><BotSkills :bot-id="bot.id" /></div>
        </section>
      </div>
    </template>

    <t-empty v-else description="未找到对应的 Bot" style="margin-top:80px" />
  </div>
</template>

<script setup>
import { ref, computed, watch, h } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { useBotStore } from '@/stores/bot'
import { botApi } from '@/api/services'
import BotBasicForm from '@/components/bot/BotBasicForm.vue'
import BotCronJobs from '@/components/bot/BotCronJobs.vue'
import BotPlatforms from '@/components/bot/BotPlatforms.vue'
import BotMemoryFiles from '@/components/bot/BotMemoryFiles.vue'
import BotAccess from '@/components/bot/BotAccess.vue'
import BotFiles from '@/components/bot/BotFiles.vue'
import BotRhythm from '@/components/bot/BotRhythm.vue'
import BotOverview from '@/components/bot/BotOverview.vue'
import BotContainer from '@/components/bot/BotContainer.vue'
import BotCompaction from '@/components/bot/BotCompaction.vue'
import BotMcp from '@/components/bot/BotMcp.vue'
import BotSkills from '@/components/bot/BotSkills.vue'
import BotHeartbeat from '@/components/bot/BotHeartbeat.vue'
import BotTerminal from '@/components/bot/BotTerminal.vue'

// 占位组件（11 项未实现面板的统一空态）
const Placeholder = {
  props: { title: String, desc: String },
  setup(p) {
    return () => h('div', { class: 'ph' }, [
      h('h3', { class: 'ph-title' }, p.title),
      h('p', { class: 'ph-desc' }, p.desc),
      h('div', { class: 'ph-box' }, '该模块正在完善中')
    ])
  }
}

const route = useRoute()
const router = useRouter()
const store = useBotStore()

const navItems = [
  { key: 'overview', label: '概览' },
  { key: 'general', label: '通用' },
  { key: 'container', label: '容器' },
  { key: 'memory', label: '记忆' },
  { key: 'platform', label: '平台' },
  { key: 'access', label: '访问控制' },
  { key: 'email', label: '邮件' },
  { key: 'terminal', label: '终端' },
  { key: 'files', label: '文件' },
  { key: 'mcp', label: 'MCP' },
  { key: 'heartbeat', label: '心跳' },
  { key: 'compact', label: '上下文压缩' },
  { key: 'rhythm', label: '聊天节奏' },
  { key: 'persona', label: '人格' },
  { key: 'cron', label: '定时任务' },
  { key: 'skills', label: '技能' }
]
const activeKey = ref('overview')

const emojis = ['🤖', '💻', '✍️', '🧠', '📊', '🎨', '🔍', '⚡', '🌟', '🚀']
const modelOptions = [
  { label: 'GPT-4o', value: 'gpt-4o' },
  { label: 'GPT-4o mini', value: 'gpt-4o-mini' },
  { label: 'Claude 3.5 Sonnet', value: 'claude-3.5-sonnet' },
  { label: 'Gemini 1.5 Pro', value: 'gemini-1.5-pro' }
]

const bot = ref(null)
const form = ref({})

const avatarText = computed(() => {
  const b = bot.value
  if (!b) return '?'
  return (b.avatar && b.avatar.length <= 2) ? b.avatar : (b.name || '?').slice(0, 2)
})

function loadBot() {
  const id = route.params.id || store.activeBotId
  let b = store.bots.find(x => x.id === id)
  if (!b && store.bots.length) b = store.bots[0]
  bot.value = b || null
  if (b) {
    form.value = {
      id: b.id, name: b.name, avatar: b.avatar || '🤖', desc: b.desc || '',
      systemPrompt: b.systemPrompt || b.prompt || '', model: b.model || 'gpt-4o',
      llmMain: b.llmMain || '', llmLight: b.llmLight || '',
      temperature: b.temperature ?? 0.7, maxTokens: b.maxTokens ?? 4096,
      workers: b.workers ?? 4, reasoningEffort: b.reasoningEffort || 'medium'
    }
  }
}
loadBot()
watch(() => route.params.id, () => { activeKey.value = 'overview'; loadBot() })

function goBack() { router.push({ name: 'system-settings' }) }

function renameBot() {
  const dlg = DialogPlugin.confirm({
    header: '重命名 Bot',
    body: () => h('input', {
      value: bot.value.name,
      class: 't-input__inner',
      style: 'width:100%;padding:8px;border:1px solid #ddd;border-radius:6px',
      onInput: e => { bot.value._tmpName = e.target.value }
    }),
    onConfirm: () => {
      const name = (bot.value._tmpName ?? bot.value.name).trim()
      if (name) { store.updateBot(bot.value.id, { name }); bot.value.name = name; form.value.name = name }
      dlg.destroy()
    }
  })
}

async function saveBasic() {
  const f = form.value
  await botApi.update(f.id, {
    name: f.name, systemPrompt: f.systemPrompt, model: f.model,
    llmMain: f.llmMain, llmLight: f.llmLight, temperature: f.temperature,
    maxTokens: f.maxTokens, workers: f.workers, reasoningEffort: f.reasoningEffort
  }).catch(() => {})
  store.updateBot(f.id, {
    name: f.name, avatar: f.avatar, desc: f.desc, model: f.model,
    temperature: f.temperature, prompt: f.systemPrompt, systemPrompt: f.systemPrompt,
    llmMain: f.llmMain, llmLight: f.llmLight, maxTokens: f.maxTokens,
    workers: f.workers, reasoningEffort: f.reasoningEffort
  })
  bot.value.name = f.name
  MessagePlugin.success('Bot 设置已保存')
}

const toggling = ref(false)
async function toggleStatus() {
  const willRun = !bot.value.running
  toggling.value = true
  try {
    if (willRun) await botApi.start(bot.value.id)
    else await botApi.stop(bot.value.id)
    bot.value.running = willRun
    store.updateBot(bot.value.id, { running: willRun, status: willRun ? 'running' : 'stopped' })
    MessagePlugin.success(willRun ? 'Bot 已启用' : 'Bot 已禁用')
  } catch (e) {
    MessagePlugin.error(e.message || '操作失败')
  } finally {
    toggling.value = false
  }
}

function removeBot() {
  const dlg = DialogPlugin.confirm({
    header: '删除 Bot', body: `确认删除「${form.value.name}」？该操作不可恢复。`, theme: 'warning',
    onConfirm: async () => {
      await botApi.remove(form.value.id).catch(() => {})
      store.deleteBot(form.value.id)
      dlg.destroy(); MessagePlugin.success('已删除'); router.push({ name: 'system-settings' })
    }
  })
}
</script>

<style scoped>
.bot-shell { flex: 1; width: 100%; min-width: 0; display: flex; flex-direction: column; height: 100%; background: #fff; }
.bot-topbar { height: 52px; display: flex; align-items: center; padding: 0 20px; border-bottom: 1px solid #f0f0f0; flex-shrink: 0; }
.back-btn { display: flex; align-items: center; gap: 4px; border: none; background: transparent; cursor: pointer; font-size: 15px; font-weight: 600; color: #1d1d1f; }
.back-btn:hover { color: #555; }

.bot-head { display: flex; align-items: center; gap: 14px; padding: 18px 24px; flex-shrink: 0; }
.bh-avatar {
  width: 56px; height: 56px; border-radius: 50%; background: #f1f1f3; overflow: hidden;
  display: flex; align-items: center; justify-content: center; font-size: 16px; color: #666; font-weight: 600;
}
.bh-avatar img { width: 100%; height: 100%; object-fit: cover; }
.bh-name-row { display: flex; align-items: center; gap: 8px; }
.bh-name { font-size: 18px; font-weight: 700; color: #1d1d1f; }
.bh-edit { color: #aaa; cursor: pointer; }
.bh-edit:hover { color: #555; }
.bh-status {
  display: inline-block; margin-top: 6px; font-size: 12px; padding: 3px 12px; border-radius: 6px;
  background: #f2f3f5; color: #888;
}
.bh-status.running { background: #1d1d1f; color: #fff; }

.bot-body { flex: 1; display: flex; min-height: 0; border-top: 1px solid #f0f0f0; }
.bot-nav {
  width: 180px; flex-shrink: 0; padding: 16px 12px; overflow-y: auto;
  border-right: 1px solid #f0f0f0; display: flex; flex-direction: column; gap: 2px;
}
.bn-item {
  text-align: left; padding: 9px 14px; border: none; background: transparent; border-radius: 8px;
  font-size: 14px; color: #666; cursor: pointer;
}
.bn-item:hover { background: #f5f5f5; }
.bn-item.active { background: #f0f1f3; color: #1d1d1f; font-weight: 600; }

.bot-content { flex: 1; min-width: 0; overflow-y: auto; padding: 24px 28px; }
.bot-content .pad { width: 100%; }

:deep(.ph) { width: 100%; }
:deep(.ph-title) { font-size: 16px; font-weight: 600; margin: 0 0 6px; }
:deep(.ph-desc) { font-size: 13px; color: #888; margin: 0 0 20px; }
:deep(.ph-box) {
  border: 1px dashed #e0e0e0; border-radius: 12px; padding: 60px; text-align: center;
  color: #bbb; font-size: 14px; background: #fafafa;
}
</style>
