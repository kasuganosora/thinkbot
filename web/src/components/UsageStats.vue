<template>
  <div class="usage" data-testid="usage-stats">
    <!-- 顶部两张图 -->
    <div class="charts-row">
      <div class="chart-card" data-testid="chart-cache-breakdown">
        <div class="chart-title">各 Bot 每日用量</div>
        <svg :viewBox="`0 0 ${W} ${H}`" class="chart-svg" preserveAspectRatio="none">
          <!-- y 轴网格 + 刻度 -->
          <g v-for="(g, i) in yGrid" :key="'g' + i">
            <line :x1="padL" :y1="g.y" :x2="W - padR" :y2="g.y" class="grid-line" />
            <text :x="padL - 8" :y="g.y + 4" class="axis-text" text-anchor="end">{{ g.label }}</text>
          </g>
          <!-- 堆叠柱：每天按 bot 堆叠 N 段 -->
          <g v-for="(d, i) in bars" :key="'bar' + i">
            <rect
              v-for="seg in d.segs"
              :key="seg.botId"
              :x="seg.x"
              :y="seg.y"
              :width="seg.w"
              :height="seg.h"
              :fill="seg.fill"
            />
          </g>
          <!-- x 轴日期 -->
          <text v-for="(t, i) in xTicks" :key="'xt' + i" :x="t.x" :y="H - 6" class="axis-text" text-anchor="middle">{{ t.label }}</text>
        </svg>
        <div class="legend">
          <span class="lg" v-for="(b, i) in botList" :key="b.id">
            <i class="dot" :style="{ background: colorOf(i) }" />{{ b.name }}
          </span>
        </div>
      </div>

      <div class="chart-card" data-testid="chart-hit-rate">
        <div class="chart-title">Cache Hit Rate</div>
        <svg :viewBox="`0 0 ${W} ${H}`" class="chart-svg" preserveAspectRatio="none">
          <g v-for="(g, i) in pctGrid" :key="'pg' + i">
            <line :x1="padL" :y1="g.y" :x2="W - padR" :y2="g.y" class="grid-line" />
            <text :x="padL - 8" :y="g.y + 4" class="axis-text" text-anchor="end">{{ g.label }}</text>
          </g>
          <polyline :points="hitLinePoints" class="hit-line" />
          <circle v-for="(p, i) in hitPoints" :key="'hp' + i" :cx="p.x" :cy="p.y" r="3" class="hit-dot" />
          <text v-for="(t, i) in xTicks" :key="'hx' + i" :x="t.x" :y="H - 6" class="axis-text" text-anchor="middle">{{ t.label }}</text>
        </svg>
      </div>
    </div>

    <!-- 按 Bot × 模型用量 -->
    <div class="panel-block" data-testid="usage-by-botmodel">
      <div class="block-head">
        <h4 class="block-title">各 Bot × 模型用量</h4>
        <span class="block-sub">直观查看每个 Bot 在每个模型上的消耗</span>
      </div>
      <t-table
        row-key="_key"
        size="small"
        hover
        :data="byBotModel"
        :columns="bmColumns"
        :loading="loadingBm"
        data-testid="botmodel-table"
      >
        <template #provider="{ row }"><span class="muted">{{ row.provider }}</span></template>
        <template #totalTokens="{ row }"><b>{{ fmt(row.totalTokens) }}</b></template>
      </t-table>
    </div>

    <!-- Records 明细 -->
    <div class="panel-block" data-testid="usage-records">
      <div class="block-head">
        <h4 class="block-title">Records</h4>
        <span class="block-sub">{{ recRangeLabel }} / {{ records.total }}</span>
      </div>
      <t-table
        row-key="id"
        size="small"
        hover
        :data="records.items"
        :columns="recColumns"
        :loading="loadingRec"
        data-testid="records-table"
      >
        <template #time="{ row }">{{ fmtTime(row.time) }}</template>
        <template #cacheReadTokens="{ row }">{{ fmtK(row.cacheReadTokens) }}</template>
        <template #inputTokens="{ row }">{{ fmtK(row.inputTokens) }}</template>
      </t-table>
      <div class="pager">
        <t-pagination
          v-model="page"
          :total="records.total"
          :page-size="pageSize"
          :show-jumper="false"
          :show-page-size="false"
          @change="loadRecords"
          data-testid="records-pager"
        />
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { statsApi } from '@/api/services'

// ---- 图表尺寸 ----
const W = 520, H = 220, padL = 56, padR = 12, padT = 14, padB = 24

const daily = ref([])
const breakdown = ref([])   // [{ date, total, segs:[{botId,botIndex,value,top}] }]
const botList = ref([])     // [{id,name}]
const byBotModel = ref([])
const records = ref({ total: 0, items: [], page: 1, pageSize: 20 })
const loadingBm = ref(false)
const loadingRec = ref(false)
const page = ref(1)
const pageSize = 20

const PALETTE = ['#4b6ef5', '#b6d94c', '#f5934b', '#9b6ef5', '#23c8b0', '#e85d8a', '#9aa0b0', '#f5c54b']
function colorOf(i) { return PALETTE[i % PALETTE.length] }

// ---- Cache Breakdown 堆叠柱（按 bot 堆叠）----
const maxStack = computed(() => {
  const m = Math.max(1, ...breakdown.value.map(d => d.total))
  return niceCeil(m)
})
const plotH = H - padT - padB
const plotW = computed(() => W - padL - padR)
const barW = computed(() => Math.max(2, (plotW.value / Math.max(1, breakdown.value.length)) * 0.7))
function barX(i) {
  const step = plotW.value / Math.max(1, breakdown.value.length)
  return padL + i * step + (step - barW.value) / 2
}
function segH(v) { return (v / maxStack.value) * plotH }
function segY(yTop) { return padT + plotH - yTop }

// 预计算每根柱子各段的纯几何数值，避免模板中做计算/解包
const bars = computed(() => {
  const n = breakdown.value.length
  const step = plotW.value / Math.max(1, n)
  const w = Math.max(2, step * 0.7)
  const max = maxStack.value
  return breakdown.value.map((d, i) => {
    const x = padL + i * step + (step - w) / 2
    const segs = d.segs.map(s => ({
      botId: s.botId,
      fill: colorOf(s.botIndex),
      x,
      w,
      y: padT + plotH - (s.top / max) * plotH,
      h: (s.value / max) * plotH
    }))
    return { segs }
  })
})

const yGrid = computed(() => {
  const lines = 5
  return Array.from({ length: lines + 1 }, (_, i) => {
    const val = (maxStack.value / lines) * i
    return { y: padT + plotH - (val / maxStack.value) * plotH, label: fmt(val) }
  })
})

// ---- Hit Rate 折线 ----
const hitRates = computed(() => daily.value.map(d => {
  const t = d.cacheHitRequests + d.cacheMissRequests
  return t ? d.cacheHitRequests / t : 0
}))
const pctGrid = computed(() => [0, 0.2, 0.4, 0.6, 0.8, 1].map(p => ({
  y: padT + plotH - p * plotH, label: Math.round(p * 100) + '%'
})))
const hitPoints = computed(() => hitRates.value.map((r, i) => {
  const step = plotW.value / Math.max(1, daily.value.length - 1)
  return { x: padL + i * step, y: padT + plotH - r * plotH }
}))
const hitLinePoints = computed(() => hitPoints.value.map(p => `${p.x},${p.y}`).join(' '))

// ---- x 轴日期刻度（取若干个） ----
const xTicks = computed(() => {
  const n = breakdown.value.length
  if (!n) return []
  const idxs = [0, Math.floor(n * 0.25), Math.floor(n * 0.5), Math.floor(n * 0.75), n - 1]
  const step = plotW.value / Math.max(1, n - 1)
  return [...new Set(idxs)].map(i => ({
    x: padL + i * step,
    label: (breakdown.value[i]?.date || '').slice(5, 10)
  }))
})

function niceCeil(v) {
  const p = Math.pow(10, Math.floor(Math.log10(v)))
  return Math.ceil(v / p) * p
}
function fmt(n) {
  n = Math.round(n)
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(n % 1_000_000 ? 1 : 0) + 'M'
  if (n >= 1000) return (n / 1000).toFixed(n % 1000 ? 1 : 0) + 'k'
  return String(n)
}
function fmtK(n) { return n >= 1000 ? (n / 1000).toFixed(1) + 'K' : String(n) }
function fmtTime(iso) {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return String(iso)
  const p = (x) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

const recRangeLabel = computed(() => {
  if (!records.value.total) return '0-0'
  const start = (page.value - 1) * pageSize + 1
  const end = Math.min(page.value * pageSize, records.value.total)
  return `${start}-${end}`
})

const bmColumns = [
  { colKey: 'botName', title: 'Bot', width: 120 },
  { colKey: 'model', title: '模型', width: 150 },
  { colKey: 'provider', title: 'Provider', width: 110 },
  { colKey: 'totalRequests', title: '请求', width: 80 },
  { colKey: 'inputTokens', title: 'Input', width: 90 },
  { colKey: 'outputTokens', title: 'Output', width: 90 },
  { colKey: 'totalTokens', title: 'Total Tokens', width: 110 },
  { colKey: 'toolCalls', title: '工具', width: 70 }
]
const recColumns = [
  { colKey: 'time', title: 'Time', width: 170 },
  { colKey: 'botName', title: 'Bot', width: 110 },
  { colKey: 'feature', title: 'Type', width: 100 },
  { colKey: 'model', title: 'Model', width: 160 },
  { colKey: 'cacheReadTokens', title: 'Cache', width: 90, align: 'right' },
  { colKey: 'inputTokens', title: 'Input', width: 90, align: 'right' },
  { colKey: 'outputTokens', title: 'Output', width: 90, align: 'right' }
]

// 构建按 bot 堆叠的每日数据：累计 top 值用于堆叠定位
function buildBreakdown(bots, series) {
  const idxOf = {}
  bots.forEach((b, i) => { idxOf[b.id] = i })
  return series.map(s => {
    let acc = 0
    const segs = bots.map(b => {
      const value = s.usage[b.id] || 0
      acc += value
      return { botId: b.id, botIndex: idxOf[b.id], value, top: acc }
    })
    return { date: s.date, total: acc, segs }
  })
}

async function loadCharts() {
  try {
    const [byBot, d] = await Promise.all([
      statsApi.dailyByBot({}),
      statsApi.dailyRange({})
    ])
    botList.value = byBot.bots
    breakdown.value = buildBreakdown(byBot.bots, byBot.series)
    daily.value = d
  } catch (e) {
    MessagePlugin.error('加载图表失败：' + (e.message || e))
  }
}
async function loadByBotModel() {
  loadingBm.value = true
  try {
    const list = await statsApi.byBotModel({})
    byBotModel.value = list.map((r, i) => ({ ...r, _key: r.botId + '|' + r.model + '|' + i }))
  } finally {
    loadingBm.value = false
  }
}
async function loadRecords() {
  loadingRec.value = true
  try {
    records.value = await statsApi.records({ page: page.value, pageSize })
  } finally {
    loadingRec.value = false
  }
}

onMounted(() => {
  loadCharts()
  loadByBotModel()
  loadRecords()
})
</script>

<style scoped>
.usage { padding: 4px 2px 24px; }
.charts-row { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-bottom: 20px; }
.chart-card { background: #fff; border: 1px solid #ececec; border-radius: 12px; padding: 16px 18px; }
.chart-title { font-size: 15px; font-weight: 600; margin-bottom: 10px; }
.chart-svg { width: 100%; height: 220px; display: block; }
.grid-line { stroke: #eee; stroke-width: 1; }
.axis-text { fill: #9aa; font-size: 10px; }
.seg-read { fill: #4b6ef5; }
.seg-write { fill: #b6d94c; }
.seg-no { fill: #9aa0b0; opacity: 0.75; }
.hit-line { fill: none; stroke: #4b6ef5; stroke-width: 2; }
.hit-dot { fill: #fff; stroke: #4b6ef5; stroke-width: 2; }
.legend { display: flex; gap: 18px; margin-top: 8px; }
.lg { display: flex; align-items: center; gap: 6px; font-size: 12px; color: #667; }
.dot { width: 10px; height: 10px; border-radius: 3px; display: inline-block; }
.dot.read { background: #4b6ef5; }
.dot.write { background: #b6d94c; }
.dot.no { background: #9aa0b0; }

.panel-block { background: #fff; border: 1px solid #ececec; border-radius: 12px; padding: 16px 18px; margin-bottom: 18px; }
.block-head { display: flex; align-items: baseline; justify-content: space-between; margin-bottom: 12px; }
.block-title { font-size: 15px; font-weight: 600; margin: 0; }
.block-sub { font-size: 12px; color: #9aa; }
.muted { color: #8a93a6; }
.pager { display: flex; justify-content: flex-end; margin-top: 12px; }
</style>
