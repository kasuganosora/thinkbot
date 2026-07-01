<template>
  <div class="bots-mgr" data-testid="bots-manager">
    <!-- 顶部：搜索 + 新建 -->
    <div class="bm-head">
      <t-input
        v-model="kw"
        class="bm-search"
        placeholder="搜索 Bot..."
        clearable
        data-testid="bots-search"
      >
        <template #prefix-icon><t-icon name="search" /></template>
      </t-input>
      <t-button theme="primary" data-testid="bots-create-btn" @click="openCreate">
        <template #icon><t-icon name="add" /></template>新建 Bot
      </t-button>
    </div>

    <!-- 卡片网格 -->
    <div v-if="filtered.length" class="bm-grid" data-testid="bots-grid">
      <div
        v-for="b in filtered"
        :key="b.id"
        class="bot-card"
        data-testid="bot-card"
        @click="openBot(b.id)"
      >
        <div class="bc-avatar">
          <img v-if="b.avatarUrl" :src="b.avatarUrl" :alt="b.name" />
          <span v-else>{{ avatarText(b) }}</span>
        </div>
        <div class="bc-main">
          <div class="bc-name">{{ b.name }}</div>
          <div class="bc-time">创建时间 {{ fmtDate(b.createdAt) }}</div>
        </div>
        <span class="bc-status" :class="{ running: b.running }">
          {{ b.running ? '运行中' : '已停止' }}
        </span>
      </div>
    </div>
    <t-empty v-else description="暂无 Bot，点击右上角「新建 Bot」创建" class="bm-empty" />

    <!-- 新建 Bot 弹窗 -->
    <t-dialog
      v-model:visible="dlgVisible"
      header="新建 Bot"
      :width="560"
      :confirm-btn="{ content: '新建 Bot', loading: creating }"
      cancel-btn="取消"
      data-testid="bots-create-dialog"
      @confirm="submit"
    >
      <t-form ref="formRef" :data="form" :rules="rules" label-align="top" class="bm-form">
        <t-form-item label="名称" name="name">
          <t-input v-model="form.name" placeholder="给你的 Bot 起个名字" data-testid="create-name" />
        </t-form-item>
        <t-form-item name="avatarUrl">
          <template #label>头像链接 <span class="lbl-opt">(可选)</span></template>
          <t-input v-model="form.avatarUrl" placeholder="输入头像图片地址" />
        </t-form-item>
        <t-form-item name="timezone">
          <template #label>时区 <span class="lbl-opt">(可选)</span></template>
          <t-input v-model="form.timezone" placeholder="继承用户或系统时区">
            <template #suffix-icon><t-icon name="time" /></template>
          </t-input>
        </t-form-item>
        <t-form-item name="securityPolicy">
          <template #label>
            安全策略
            <t-tooltip content="控制该 Bot 允许处理的会话类型范围">
              <t-icon name="help-circle" class="lbl-help" />
            </t-tooltip>
          </template>
          <t-select v-model="form.securityPolicy" :options="policyOptions" />
          <div class="form-hint">{{ policyHint }}</div>
        </t-form-item>
      </t-form>
      <div class="dlg-note">首次创建时可能需要拉取基础镜像，提交后请耐心等待片刻。</div>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { MessagePlugin } from 'tdesign-vue-next'
import { useBotStore } from '@/stores/bot'
import { botApi } from '@/api/services'

const router = useRouter()
const store = useBotStore()

onMounted(() => { store.fetchBots() })

const kw = ref('')
const filtered = computed(() => {
  const q = kw.value.trim().toLowerCase()
  if (!q) return store.bots
  return store.bots.filter(b => (b.name || '').toLowerCase().includes(q))
})

function avatarText(b) {
  return (b.avatar && b.avatar.length <= 2) ? b.avatar : (b.name || '?').slice(0, 2)
}
function fmtDate(v) {
  if (!v) return '--'
  const d = new Date(v)
  if (isNaN(d)) return String(v)
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`
}

function openBot(id) {
  store.selectBot(id)
  router.push({ name: 'bot-settings', params: { id } })
}

// ---- 新建弹窗 ----
const dlgVisible = ref(false)
const creating = ref(false)
const formRef = ref()
const policyOptions = [
  { label: '允许全部', value: 'allow_all' },
  { label: '仅白名单', value: 'whitelist' },
  { label: '仅私聊', value: 'private_only' }
]
const policyHints = {
  allow_all: '不额外添加限制，默认允许所有会话类型。',
  whitelist: '仅处理白名单内来源的会话，其余忽略。',
  private_only: '仅处理一对一私聊会话，群聊将被忽略。'
}
const defaultForm = () => ({ name: '', avatarUrl: '', timezone: '', securityPolicy: 'allow_all' })
const form = ref(defaultForm())
const policyHint = computed(() => policyHints[form.value.securityPolicy] || '')
const rules = {
  name: [{ required: true, message: '请输入 Bot 名称', type: 'error' }]
}

function openCreate() {
  form.value = defaultForm()
  dlgVisible.value = true
}

async function submit(ctx) {
  const valid = await formRef.value.validate()
  if (valid !== true) return
  creating.value = true
  try {
    const payload = {
      name: form.value.name.trim(),
      avatarUrl: form.value.avatarUrl.trim(),
      timezone: form.value.timezone.trim(),
      securityPolicy: form.value.securityPolicy
    }
    // store.createBot 内部已调用 botApi.create 创建后端记录
    await store.createBot(payload)
    MessagePlugin.success('Bot 已创建')
    dlgVisible.value = false
  } finally {
    creating.value = false
  }
}
</script>

<style scoped>
.bots-mgr {
  display: flex;
  flex-direction: column;
  height: 100%;
}
.bm-head {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
  margin-bottom: 20px;
}
.bm-search { width: 280px; }
.bm-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 16px;
  align-content: start;
  overflow-y: auto;
}
.bot-card {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 18px 20px;
  border: 1px solid #ececec;
  border-radius: 14px;
  background: #fff;
  cursor: pointer;
  transition: box-shadow 0.15s, border-color 0.15s;
}
.bot-card:hover {
  border-color: #d9d9d9;
  box-shadow: 0 4px 14px rgba(0, 0, 0, 0.06);
}
.bc-avatar {
  width: 44px;
  height: 44px;
  border-radius: 50%;
  background: #f1f1f3;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 14px;
  color: #666;
  overflow: hidden;
}
.bc-avatar img { width: 100%; height: 100%; object-fit: cover; }
.bc-main { flex: 1; min-width: 0; }
.bc-name {
  font-size: 15px;
  font-weight: 600;
  color: #1d1d1f;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.bc-time { font-size: 12px; color: #999; margin-top: 4px; }
.bc-status {
  flex-shrink: 0;
  font-size: 12px;
  padding: 3px 10px;
  border-radius: 6px;
  background: #f2f3f5;
  color: #888;
}
.bc-status.running {
  background: #1d1d1f;
  color: #fff;
}
.bm-empty { margin-top: 60px; }

/* 弹窗表单 */
.bm-form { padding-top: 4px; }
.lbl-opt { color: #aaa; font-weight: 400; margin-left: 4px; }
.lbl-help { color: #bbb; margin-left: 4px; cursor: help; }
.form-hint { font-size: 12px; color: #999; margin-top: 6px; }
.dlg-note {
  margin-top: 4px;
  padding: 12px 14px;
  background: #f7f8fa;
  border-radius: 8px;
  font-size: 13px;
  color: #999;
}
</style>
