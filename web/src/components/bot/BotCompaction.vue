<template>
  <div class="compact-wrap" data-testid="bot-compaction">
    <!-- 配置区 -->
    <div class="cfg">
      <div class="cfg-head">
        <div>
          <div class="ch-title">启用上下文压缩</div>
          <div class="ch-desc">上下文过大时自动摘要旧消息以节省 token</div>
        </div>
        <t-switch v-model="cfg.enabled" size="large" />
      </div>

      <div class="cfg-grid">
        <!-- 压缩阈值 -->
        <div class="field full">
          <label class="lbl">压缩阈值（输入 token 数）</label>
          <t-input-number
            v-model="cfg.threshold"
            :min="0"
            :step="1024"
            theme="normal"
            :disabled="!cfg.enabled"
            style="width: 100%"
          />
        </div>

        <!-- 压缩比例 -->
        <div class="field full">
          <label class="lbl">压缩比例（%）</label>
          <div class="sub">压缩较旧消息的百分比，剩余的最近消息保持原始完整度。</div>
          <div class="ratio-row">
            <t-slider
              v-model="cfg.ratio"
              :min="0"
              :max="100"
              :disabled="!cfg.enabled"
              class="ratio-slider"
            />
            <span class="ratio-val">{{ cfg.ratio }}%</span>
          </div>
        </div>
      </div>

      <!-- 压缩模型 -->
      <div class="field full">
        <label class="lbl">压缩模型</label>
        <div class="sub">选择用于摘要的模型，未设置时默认使用聊天模型。</div>
        <t-select
          v-model="cfg.model"
          filterable
          clearable
          placeholder="默认使用聊天模型"
          :disabled="!cfg.enabled"
          :options="modelOptions"
        />
      </div>

      <div class="cfg-foot">
          <t-button theme="default" :loading="saving" @click="save">保存设置</t-button>
      </div>
    </div>

    <div class="divider"></div>

    <!-- 压缩记录 -->
    <div class="hist">
      <div class="hist-head">
        <span class="hh-title">压缩记录</span>
        <t-space size="small">
          <t-select v-model="filter" :options="filterOptions" size="small" style="width: 120px" @change="loadHistory" />
          <t-button variant="outline" size="small" @click="clearHistory">清空记录</t-button>
          <t-button variant="outline" size="small" @click="loadHistory">
            <template #icon><t-icon name="refresh" /></template>
            刷新
          </t-button>
        </t-space>
      </div>

      <t-table
        :data="history"
        :columns="columns"
        row-key="id"
        :loading="histLoading"
        size="medium"
      >
        <template #status="{ row }">
          <t-tag :theme="row.status === 'success' ? 'success' : 'danger'" variant="light">
            {{ row.status === 'success' ? '成功' : '失败' }}
          </t-tag>
        </template>
        <template #cost="{ row }">{{ row.cost.toFixed(1) }}s</template>
        <template #error="{ row }">
          <span :class="{ 'err-text': row.error }">{{ row.error || '—' }}</span>
        </template>
      </t-table>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botCompactionApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const modelOptions = [
  { label: 'deepseek-v4-flash', value: 'deepseek-v4-flash' },
  { label: 'gpt-4o-mini', value: 'gpt-4o-mini' },
  { label: 'claude-3.5-haiku', value: 'claude-3.5-haiku' },
  { label: 'gemini-1.5-flash', value: 'gemini-1.5-flash' }
]

const cfg = reactive({ enabled: true, threshold: 131072, ratio: 37, model: '' })
const saving = ref(false)

async function loadConfig() {
  const c = await botCompactionApi.getConfig(props.botId)
  Object.assign(cfg, c)
}

async function save() {
  saving.value = true
  try {
    await botCompactionApi.updateConfig(props.botId, { ...cfg })
    MessagePlugin.success('设置已保存')
  } catch (e) {
    MessagePlugin.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

/* ---------------- 压缩记录 ---------------- */
const filter = ref('all')
const filterOptions = [
  { label: '全部', value: 'all' },
  { label: '成功', value: 'success' },
  { label: '失败', value: 'failed' }
]
const columns = [
  { colKey: 'status', title: '状态', width: 120 },
  { colKey: 'time', title: '时间', width: 240 },
  { colKey: 'cost', title: '耗时', width: 140 },
  { colKey: 'error', title: '错误' }
]
const history = ref([])
const histLoading = ref(false)

async function loadHistory() {
  histLoading.value = true
  try {
    const res = await botCompactionApi.history(props.botId, filter.value)
    history.value = res.records || []
  } finally {
    histLoading.value = false
  }
}

function clearHistory() {
  const dlg = DialogPlugin.confirm({
    header: '清空压缩记录',
    body: '确认清空全部压缩记录？该操作不可恢复。',
    theme: 'warning',
    onConfirm: async () => {
      await botCompactionApi.clearHistory(props.botId)
      dlg.destroy()
      MessagePlugin.success('已清空')
      loadHistory()
    }
  })
}

onMounted(() => { loadConfig(); loadHistory() })
</script>

<style scoped>
.compact-wrap { width: 100%; }

/* 配置区 */
.cfg-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 20px; }
.ch-title { font-size: 15px; font-weight: 600; color: #1d1d1f; }
.ch-desc { font-size: 13px; color: #888; margin-top: 4px; }

.cfg-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 20px 24px; margin-bottom: 20px; }
.field { display: flex; flex-direction: column; }
.field.full { grid-column: auto; }
.lbl { font-size: 13px; font-weight: 600; color: #1d1d1f; margin-bottom: 8px; }
.sub { font-size: 12px; color: #999; margin: -2px 0 10px; }

.ratio-row { display: flex; align-items: center; gap: 16px; }
.ratio-slider { flex: 1; min-width: 0; }
.ratio-val { width: 44px; text-align: right; font-size: 14px; color: #1d1d1f; font-variant-numeric: tabular-nums; }

.cfg-foot { display: flex; justify-content: flex-end; margin-top: 8px; }

.divider { height: 1px; background: #f0f0f0; margin: 28px 0; }

/* 记录区 */
.hist-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 16px; }
.hh-title { font-size: 16px; font-weight: 600; color: #1d1d1f; }
.err-text { color: #c0392b; }
</style>
