<template>
  <div>
    <t-card title="梦境巩固配置" :bordered="false" class="card">
      <t-loading :loading="loading">
        <t-form label-align="top">
          <t-form-item label="启用梦境巩固">
            <t-switch v-model="config.enabled" data-testid="dreaming-enabled" />
            <span class="tip">定期对长期记忆进行整理、去重与提炼。</span>
          </t-form-item>
          <t-form-item label="调度计划（Cron）">
            <t-input v-model="config.schedule" placeholder="如 0 3 * * *" style="width: 280px" data-testid="dreaming-schedule" />
          </t-form-item>
        </t-form>
        <t-space>
          <t-button theme="primary" @click="save" data-testid="dreaming-save-btn">保存配置</t-button>
          <t-button
            variant="outline"
            :disabled="!config.enabled || triggering"
            :loading="triggering"
            @click="trigger"
            data-testid="dreaming-trigger-btn"
          >{{ triggering ? '巩固进行中…' : '立即触发一次' }}</t-button>
        </t-space>
      </t-loading>
    </t-card>

    <t-alert v-if="triggering" theme="info" class="card running-tip">
      梦境巩固正在后台执行（通常 30 秒 ~ 2 分钟，取决于记忆条数与模型响应），完成后会自动展示结果，请勿关闭页面。
    </t-alert>

    <t-card v-if="status && status.enabled" title="运行状态" :bordered="false" class="card">
      <t-descriptions :column="2" bordered>
        <t-descriptions-item label="运行中">{{ status.running ? '是' : '否' }}</t-descriptions-item>
        <t-descriptions-item label="状态">{{ status.cronJob?.state || '-' }}</t-descriptions-item>
        <t-descriptions-item label="下次运行">{{ formatTime(status.cronJob?.nextRunAt) }}</t-descriptions-item>
        <t-descriptions-item label="上次运行">{{ formatTime(status.lastRun?.runAt || status.cronJob?.lastRunAt) }}</t-descriptions-item>
        <t-descriptions-item label="上次结果">{{ status.lastRun?.summary || status.cronJob?.lastResult || '-' }}</t-descriptions-item>
        <t-descriptions-item label="累计运行">{{ status.lastRun?.runCount ?? status.cronJob?.runCount ?? 0 }} 次</t-descriptions-item>
      </t-descriptions>
    </t-card>

    <t-card v-if="lastReport" title="最近一次巩固结果" :bordered="false" class="card">
      <t-descriptions :column="3" bordered size="small">
        <t-descriptions-item label="浅层摄入">{{ lastReport.lightIngested }}</t-descriptions-item>
        <t-descriptions-item label="去重">{{ lastReport.lightDeduped }}</t-descriptions-item>
        <t-descriptions-item label="丢弃">{{ lastReport.lightDropped }}</t-descriptions-item>
        <t-descriptions-item label="REM 主题">{{ lastReport.remThemes }}</t-descriptions-item>
        <t-descriptions-item label="深层评分">{{ lastReport.deepScored }}</t-descriptions-item>
        <t-descriptions-item label="深层晋升">{{ lastReport.deepPromoted }}</t-descriptions-item>
        <t-descriptions-item label="耗时">{{ lastDuration }}</t-descriptions-item>
        <t-descriptions-item label="阶段">{{ lastPhase }}</t-descriptions-item>
      </t-descriptions>
      <div v-if="lastRunAt" class="run-at">运行于 {{ formatTime(lastRunAt) }}</div>
    </t-card>

    <t-card v-if="status && status.enabled" title="最近晋升的记忆" :bordered="false" class="card">
      <p class="tip">每次梦境巩固把一条记忆提升为长期记忆时，都会记录<strong>提升理由</strong>（达标分数 / 信号明细 / 通过的门控）、<strong>引用的原始 L0 条目原文预览</strong>与<strong>完整原内容</strong>，便于审计「为什么被记住、凭什么原文」。运行一次巩固后会自动刷新。</p>
      <t-loading :loading="promotionsLoading">
        <t-alert v-if="promotionRows.length === 0" theme="info" class="card">还没有被梦境巩固提升的长期记忆。运行一次巩固后，这里会逐条展示「为什么被记住、凭什么原文」。</t-alert>
        <t-table
          v-else
          :data="promotionRows"
          :columns="promotionCols"
          size="small"
          row-key="id"
          class="card"
          :expanded-row-keys="expandedRows"
          @expand-change="onExpandChange"
        >
          <template #summary="{ row }">
            <div class="summary-main">{{ row.summary }}</div>
            <div v-if="row.sourcePreview" class="summary-preview">{{ row.sourcePreview }}</div>
          </template>
          <template #source_entries="{ row }">
            <t-tag v-if="row.sourceCount === 0" variant="light" size="small">无</t-tag>
            <t-tag v-else variant="light" size="small" theme="primary">引用 {{ row.sourceCount }} 条（点此行展开）</t-tag>
          </template>
          <template #expandedRow="{ row }">
            <div class="source-list">
              <div v-for="(s, i) in row.sourceEntries" :key="s.id" class="source-item">
                <div class="source-meta">
                  <span class="source-idx">#{{ i + 1 }}</span>
                  <t-tag v-if="s.speaker" size="small" variant="light">{{ speakerLabel(s.speaker) }}</t-tag>
                  <span class="source-id" :title="s.id">{{ s.id }}</span>
                </div>
                <div class="source-content">{{ s.content }}</div>
              </div>
              <div v-if="!row.sourceEntries || row.sourceEntries.length === 0" class="source-empty">
                无引用的原始 L0 条目（可能已过期，或本轮无来源）
              </div>
            </div>
          </template>
        </t-table>
      </t-loading>
    </t-card>

    <t-card title="记忆清理（运维）" :bordered="false" class="card">
      <p class="tip">清理历史存量里不符合标准的短噪声记忆（少于 5 字符或 5 个词）。新写入已被源头拦截，此处处理存量垃圾。扫描范围是库中该层级<b>全部</b>条目（不受内存容量上限影响）。删除为破坏性操作，建议先预览再清理。</p>
      <t-form label-align="top">
        <t-form-item label="目标层级">
          <t-checkbox-group v-model="cleanTiers" data-testid="clean-tier-group">
            <t-checkbox value="L0">L0 工作记忆</t-checkbox>
            <t-checkbox value="L1">L1 长期记忆</t-checkbox>
            <t-checkbox value="L2">L2 场景</t-checkbox>
            <t-checkbox value="L3">L3 画像</t-checkbox>
          </t-checkbox-group>
        </t-form-item>
      </t-form>
      <t-space>
        <t-button
          theme="default"
          :loading="cleaning"
          :disabled="cleanTiers.length === 0"
          data-testid="clean-preview-btn"
          @click="previewTrivial"
        >预览垃圾</t-button>
        <t-button
          theme="danger"
          :loading="cleaning"
          :disabled="cleanTiers.length === 0"
          data-testid="clean-confirm-btn"
          @click="confirmClean"
        >确认清理</t-button>
      </t-space>

      <t-alert v-if="cleanResult" :theme="cleanResult.dryRun ? 'info' : 'success'" class="card">
        扫描 {{ cleanResult.scanned }} 条，命中垃圾 {{ cleanResult.matched }} 条
        <template v-if="!cleanResult.dryRun">，已删除 {{ cleanResult.deleted }} 条</template>
        <span v-for="(st, t) in cleanResult.byTier" :key="t"> ｜ {{ t }}: 扫描 {{ st.scanned }} / 命中 {{ st.matched }}</span>
      </t-alert>

      <t-table
        v-if="cleanResult && cleanResult.sample.length"
        :data="cleanResult.sample"
        :columns="cleanCols"
        size="small"
        row-key="id"
        class="card"
      />
    </t-card>

    <t-card title="重复刷屏清理（运维）" :bordered="false" class="card">
      <p class="tip">清理同一 (层级+范围+内容) 精确重复 ≥ N 次的条目，每组保留 1 条、删除其余。典型如 bot 自动刷屏帖（"今日の迷路です！ #AiMaze" ×15）。默认 N=5，只命中纯刷屏，放过出现 3~4 次的真实记忆。扫描范围是库中该层级<b>全部</b>条目。删除为破坏性操作，建议先预览再清理。</p>
      <t-form label-align="top">
        <t-form-item label="目标层级">
          <t-checkbox-group v-model="dupTiers" data-testid="dup-tier-group">
            <t-checkbox value="L0">L0 工作记忆</t-checkbox>
            <t-checkbox value="L1">L1 长期记忆</t-checkbox>
            <t-checkbox value="L2">L2 场景</t-checkbox>
            <t-checkbox value="L3">L3 画像</t-checkbox>
          </t-checkbox-group>
        </t-form-item>
        <t-form-item label="重复阈值 N（同一内容出现次数 ≥ N 才清理）">
          <t-input-number v-model="dupMinCount" :min="2" :max="100" style="width: 160px" data-testid="dup-mincount" />
        </t-form-item>
      </t-form>
      <t-space>
        <t-button
          theme="default"
          :loading="duping"
          :disabled="dupTiers.length === 0"
          data-testid="dup-preview-btn"
          @click="previewDuplicates"
        >预览重复</t-button>
        <t-button
          theme="danger"
          :loading="duping"
          :disabled="dupTiers.length === 0"
          data-testid="dup-confirm-btn"
          @click="confirmDupClean"
        >确认清理</t-button>
      </t-space>

      <t-alert v-if="dupResult" :theme="dupResult.dryRun ? 'info' : 'success'" class="card">
        扫描 {{ dupResult.scanned }} 条，命中 {{ dupResult.duplicateGroups }} 组重复（将删除 {{ dupResult.toDelete }} 条）
        <template v-if="!dupResult.dryRun">，已删除 {{ dupResult.deleted }} 条</template>
        <span v-for="(st, t) in dupResult.byTier" :key="t"> ｜ {{ t }}: 扫描 {{ st.scanned }} / 删 {{ st.matched }}</span>
      </t-alert>

      <t-table
        v-if="dupResult && dupResult.groups.length"
        :data="dupResult.groups"
        :columns="dupCols"
        size="small"
        row-key="keepId"
        class="card"
      />
    </t-card>
  </div>
</template>

<script setup>
import { computed, ref, watch } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { dreamingApi, memoryApi } from '@/api/services'
import { formatTime } from '@/utils/format'

const props = defineProps({ botId: { type: String, required: true } })

const loading = ref(false)
const triggering = ref(false)
const config = ref({ enabled: false, schedule: '0 3 * * *' })
const status = ref(null)
const lastTrigger = ref(null)

// 「最近一次巩固结果」优先读服务端持久化记录（刷新页面、重启服务后依然在），
// 仅在刚触发完、status 尚未刷到记录时回退到本次触发响应。
const lastReport = computed(() => status.value?.lastRun?.report || lastTrigger.value || null)
const lastDuration = computed(() => status.value?.lastRun?.duration || lastTrigger.value?.duration || '-')
const lastPhase = computed(() => status.value?.lastRun?.phase || lastTrigger.value?.phase || '-')
const lastRunAt = computed(() => status.value?.lastRun?.runAt || null)

// ── 最近晋升的记忆：每次提升都附「理由 + 引用的原 L0 条目」 ──
const promotions = ref([])
const promotionsLoading = ref(false)
async function loadPromotions() {
  promotionsLoading.value = true
  try {
    const res = await dreamingApi.promotions(props.botId, 30)
    promotions.value = res.promotions || []
  } catch (e) {
    // 非致命：面板留空，不阻塞其余展示。
    promotions.value = []
  } finally {
    promotionsLoading.value = false
  }
}

// 把后端嵌套结构拍平成表格行：理由摘要 + 原文预览 + 引用的原 L0 条目快照 + 晋升时间。
const promotionRows = computed(() => promotions.value.map((p) => ({
  id: p.id,
  content: p.content,
  score: typeof p.score === 'number' ? p.score.toFixed(2) : p.score,
  summary: p.reason?.summary || '—',
  sourcePreview: p.reason?.source_preview || '',
  sourceIDs: p.source_ids || [],
  sourceEntries: p.source_entries || [],
  sourceCount: (p.source_entries || []).length,
  createdAt: p.createdAt,
})))
const promotionCols = [
  { colKey: 'content', title: '记忆内容', ellipsis: true },
  { colKey: 'score', title: '得分', width: 70 },
  { colKey: 'summary', title: '提升理由', ellipsis: true },
  { colKey: 'source_entries', title: '引用原条目', width: 150 },
  { colKey: 'createdAt', title: '晋升时间', width: 170, cell: ({ row }) => formatTime(row.createdAt) },
]

// 展开行：展示每条被引用的原始 L0 工作记忆内容（快照，永久可读）。
const expandedRows = ref([])
function onExpandChange(keys) {
  expandedRows.value = keys
}
function speakerLabel(spk) {
  switch (spk) {
    case 'user': return '用户原话'
    case 'assistant': return 'Bot 回复'
    case 'observer': return '公开观察'
    default: return '未知来源'
  }
}

async function load() {
  loading.value = true
  try {
    config.value = await dreamingApi.getConfig(props.botId)
    status.value = await dreamingApi.status(props.botId)
    await loadPromotions()
  } finally {
    loading.value = false
  }
}
watch(() => props.botId, load, { immediate: true })

async function save() {
  await dreamingApi.updateConfig(props.botId, { enabled: config.value.enabled, schedule: config.value.schedule })
  MessagePlugin.success('梦境配置已保存，需重启 Bot 才生效')
  status.value = await dreamingApi.status(props.botId)
}

async function trigger() {
  if (triggering.value) return
  // 后端是同步执行整条梦境管线（实测 30s ~ 2min），期间必须给出明确反馈，
  // 否则按钮看起来"点了没反应"（踩过一次）。
  triggering.value = true
  const tip = MessagePlugin.info('梦境巩固已开始，正在整理记忆，完成后会显示结果…', 0)
  try {
    lastTrigger.value = await dreamingApi.trigger(props.botId)
    MessagePlugin.success('梦境巩固完成')
    status.value = await dreamingApi.status(props.botId)
    await load()
  } catch (e) {
    MessagePlugin.error('触发失败：' + (e.message || '请稍后重试'))
  } finally {
    triggering.value = false
    tip && tip.close && tip.close()
  }
}

// ── 记忆清理（运维）：清理历史存量里的短噪声记忆 ──
// 判定口径与后端 memory.IsTrivialMemoryContent 一致（<5 字符或 <5 词）。
const cleanTiers = ref(['L0', 'L1'])
const cleaning = ref(false)
const cleanResult = ref(null)
const cleanCols = [
  { colKey: 'tier', title: '层级', width: 80 },
  { colKey: 'scope', title: '范围', width: 160 },
  { colKey: 'content', title: '内容', ellipsis: true },
]

async function runCleanup(dryRun) {
  if (cleaning.value) return
  if (cleanTiers.value.length === 0) {
    MessagePlugin.warning('请至少选择一个目标层级')
    return
  }
  cleaning.value = true
  try {
    const res = await memoryApi.cleanupTrivial(props.botId, { tiers: cleanTiers.value, dryRun })
    // sample 后端无命中时可能为 null，统一兜成数组，模板才能安全取 length。
    cleanResult.value = { ...res, dryRun, sample: res.sample || [] }
    if (dryRun) {
      MessagePlugin.info(`预览完成：扫描 ${res.scanned} 条，命中垃圾 ${res.matched} 条（未删除任何数据）`)
    } else {
      MessagePlugin.success(`清理完成：已删除 ${res.deleted} 条垃圾记忆`)
      // 真删除后刷新状态，避免页面残留旧统计。
      try {
        status.value = await dreamingApi.status(props.botId)
      } catch (e) {
        // 状态刷新失败不影响清理结果展示，忽略即可。
      }
    }
  } catch (e) {
    MessagePlugin.error((dryRun ? '预览失败：' : '清理失败：') + (e.message || '请稍后重试'))
  } finally {
    cleaning.value = false
  }
}

function previewTrivial() {
  runCleanup(true)
}

function confirmClean() {
  // 破坏性操作：必须二次确认，并提示先预览。
  const dlg = DialogPlugin.confirm({
    header: '确认清理垃圾记忆',
    body: `将从 ${cleanTiers.value.join('、')} 中永久删除不符合标准的短噪声记忆（少于 5 字符或 5 个词），该操作不可撤销。建议先点「预览垃圾」确认清单。`,
    confirmBtn: { content: '确认删除', theme: 'danger' },
    cancelBtn: '取消',
    onConfirm: () => {
      runCleanup(false)
      dlg.destroy()
    },
    onCancel: () => dlg.destroy(),
  })
}

// ── 重复刷屏清理（运维）：清理同一 (层级+范围+内容) 精确重复 ≥N 次的条目 ──
// 判定口径与后端 handleCleanupDuplicateMemory 一致：按 (tier, scope, content) 分组计数，
// 每组保留 1 条、删除其余（"去重"而非"全删"）。
const dupTiers = ref(['L0', 'L1'])
const dupMinCount = ref(5)
const duping = ref(false)
const dupResult = ref(null)
const dupCols = [
  { colKey: 'tier', title: '层级', width: 80 },
  { colKey: 'scope', title: '范围', width: 160 },
  { colKey: 'count', title: '重复次数', width: 100 },
  { colKey: 'content', title: '内容（保留 1 条，其余删除）', ellipsis: true },
]

async function runDupCleanup(dryRun) {
  if (duping.value) return
  if (dupTiers.value.length === 0) {
    MessagePlugin.warning('请至少选择一个目标层级')
    return
  }
  duping.value = true
  try {
    const res = await memoryApi.cleanupDuplicates(props.botId, {
      tiers: dupTiers.value,
      minCount: dupMinCount.value,
      dryRun,
    })
    // groups 后端无命中时可能为 null，统一兜成数组，模板才能安全取 length。
    dupResult.value = { ...res, dryRun, groups: res.groups || [] }
    if (dryRun) {
      MessagePlugin.info(`预览完成：扫描 ${res.scanned} 条，命中 ${res.duplicateGroups} 组重复，将删除 ${res.toDelete} 条（未删除任何数据）`)
    } else {
      MessagePlugin.success(`清理完成：已删除 ${res.deleted} 条重复记忆`)
      try {
        status.value = await dreamingApi.status(props.botId)
      } catch (e) {
        // 状态刷新失败不影响清理结果展示，忽略即可。
      }
    }
  } catch (e) {
    MessagePlugin.error((dryRun ? '预览失败：' : '清理失败：') + (e.message || '请稍后重试'))
  } finally {
    duping.value = false
  }
}

function previewDuplicates() {
  runDupCleanup(true)
}

function confirmDupClean() {
  const dlg = DialogPlugin.confirm({
    header: '确认清理重复记忆',
    body: `将删除 ${dupTiers.value.join('、')} 中同一内容出现 ≥ ${dupMinCount.value} 次的重复条目（每组保留 1 条），该操作不可撤销。建议先点「预览重复」确认清单。`,
    confirmBtn: { content: '确认删除', theme: 'danger' },
    cancelBtn: '取消',
    onConfirm: () => {
      runDupCleanup(false)
      dlg.destroy()
    },
    onCancel: () => dlg.destroy(),
  })
}
</script>

<style scoped>
.card { margin-bottom: 20px; }
.tip { margin-left: 12px; color: var(--bp-label-tertiary); font-size: 13px; }
.run-at { margin-top: 8px; color: var(--bp-label-tertiary); font-size: 12px; }
.source-list { padding: 8px 4px; }
.summary-main { line-height: 1.5; }
.summary-preview { margin-top: 4px; padding: 4px 8px; background: var(--bp-bg-secondary, #f3f3f3); border-left: 3px solid var(--bp-border-brand, #0052d9); border-radius: 3px; font-size: 12px; color: var(--bp-label-secondary); white-space: pre-wrap; word-break: break-word; line-height: 1.6; }
.source-item { padding: 8px 0; border-bottom: 1px solid var(--bp-border-secondary); }
.source-item:last-child { border-bottom: none; }
.source-meta { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; }
.source-idx { font-weight: 600; color: var(--bp-label-secondary); }
.source-id { font-size: 12px; color: var(--bp-label-tertiary); font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.source-content { white-space: pre-wrap; word-break: break-word; line-height: 1.6; color: var(--bp-label-primary); }
.source-empty { color: var(--bp-label-tertiary); font-size: 13px; }
</style>
