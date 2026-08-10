<template>
  <div class="tp-wrap" data-testid="bot-tool-perm">
    <!-- 页头 -->
    <div class="tp-head">
      <h3 class="tp-title">工具权限</h3>
      <p class="tp-desc">按「工具 / 平台 / 用户」控制此 Bot 可用的工具。规则按排序升序评估，首条匹配者生效。</p>
    </div>

    <!-- 判定规则说明 -->
    <div class="tp-card tp-explain">
      <div class="card-title">判定方式</div>
      <div class="ex-grid">
        <div class="ex-item">
          <div class="ex-badge ex-open"><t-icon name="check-circle" /></div>
          <div class="ex-body">
            <div class="ex-h">平台无规则 → 全部开放</div>
            <div class="ex-t">某平台一条规则都没有时，该平台下所有工具默认可用。未被约束的渠道不会被锁死。</div>
          </div>
        </div>
        <div class="ex-item">
          <div class="ex-badge ex-lock"><t-icon name="lock-on" /></div>
          <div class="ex-body">
            <div class="ex-h">平台有规则但未命中 → 仅禁敏感工具</div>
            <div class="ex-t">为某平台配了规则后，<b>敏感工具</b>（联网/ 命令执行 / 文件读写 / 子智能体）进入白名单模式；<b>基础工具</b>（计算、文本、记忆、只读查询）仍默认开放，Bot 不会失去基本能力。</div>
          </div>
        </div>
        <div class="ex-item">
          <div class="ex-badge ex-order"><t-icon name="order-ascending" /></div>
          <div class="ex-body">
            <div class="ex-h">首条匹配生效</div>
            <div class="ex-t">按排序升序遍历，第一条同时命中「工具 + 平台 + 用户」的规则决定结果，后续规则不再评估。</div>
          </div>
        </div>
        <div class="ex-item">
          <div class="ex-badge ex-star"><t-icon name="system-code" /></div>
          <div class="ex-body">
            <div class="ex-h"><code>*</code> 通配</div>
            <div class="ex-t">工具 / 平台 / 用户均支持 <code>*</code> 匹配全部；工具还支持前后缀，如 <code>sandbox_*</code>。</div>
          </div>
        </div>
      </div>
      <div class="ex-note">
        <t-icon name="info-circle" />
        <span>系统内部会话（定时任务 / 心跳 / 梦境巩固）不受这些规则约束，始终可使用全部工具。若确实要连基础工具一起锁死，配一条 <code>工具 = *</code> 的「禁止」规则即可 —— 显式规则优先级高于默认开放。</span>
      </div>
    </div>

    <!-- 渠道发言权限（只看不发） -->
    <div class="tp-card">
      <div class="card-title">
        渠道发言
        <span class="ob-badge">独立于工具权限</span>
      </div>
      <div class="card-sub">
        关闭发言后，Bot 在该渠道<b>被@ 也不会回复</b>，但仍照常接收消息、思考、记忆。
        这是「潜水/ 只看不发」模式 —— Bot 的自动回复走消息出站链路，不受下方工具规则约束，因此需要单独开关。
      </div>

      <div v-if="obLoading" class="ob-loading">
        <t-icon name="loading" class="spin" /> 加载中…
      </div>
      <div v-else class="ob-grid">
        <div
          v-for="o in outbound"
          :key="o.platform"
          class="ob-item"
          :class="{ 'is-mute': o.readOnly }"
        >
          <div class="ob-left">
            <span class="ob-plat" :class="`pf-${platClass(o.platform)}`">
              {{ o.platform === '*' ? '全部渠道' : o.platform }}
            </span>
            <span class="ob-state">{{ o.readOnly ? '只看不发' : '可发言' }}</span>
          </div>
          <t-switch
            size="small"
            :value="!o.readOnly"
            :loading="obSaving === o.platform"
            @change="(v) => toggleOutbound(o, v)"
          />
        </div>
      </div>
      <div class="ob-note">
        <t-icon name="error-circle" />
        <span>
          「全部渠道」开关会一次性覆盖所有渠道；单渠道开关只影响该渠道。
          若要禁止 Bot <b>主动</b>发帖但保留被动回复，请改用下方工具规则禁掉
          <code>misskey_create_*</code> 等发言类工具。
        </span>
      </div>
    </div>

    <!-- 规则列表 -->
    <div class="tp-card">
      <div class="tp-rule-head">
        <div>
          <div class="card-title">
            规则
            <span v-if="rules.length" class="count-pill">{{ rules.length }}</span>
          </div>
          <div class="card-sub">拖动左侧手柄可调整优先级，也可用上移/ 下移按钮 —— 排序数字越小越先评估。禁止规则需排在允许规则之上才会生效。</div>
        </div>
        <div class="tp-head-ops">
          <t-button theme="default" variant="outline" :loading="resetting" @click="confirmReset">
            <template #icon><t-icon name="rollback" /></template>恢复默认
          </t-button>
          <t-button theme="primary" @click="openAdd">
            <template #icon><t-icon name="add" /></template>添加规则
          </t-button>
        </div>
      </div>

      <!-- 平台覆盖概览 -->
      <div v-if="!loading && platformSummary.length" class="plat-summary">
        <span
          v-for="p in platformSummary"
          :key="p.name"
          class="ps-chip"
          :class="p.constrained ? 'ps-limited' : 'ps-open'"
          :title="p.constrained ? `${p.name}：已有 ${p.count} 条启用规则，未命中的敏感工具将被禁止（基础工具仍开放）` : `${p.name}：无启用规则，全部工具开放`"
        >
          <i class="ps-dot" />
          {{ p.name }}
          <em>{{ p.constrained ? `管控中 · ${p.count}` : '全开放' }}</em>
        </span>
      </div>

      <!-- 加载骨架 -->
      <div v-if="loading" class="tp-skeleton">
        <div v-for="i in 3" :key="i" class="sk-row" />
      </div>

      <!-- 表格 -->
      <div v-else-if="rules.length" class="tp-table">
        <div class="tp-tr tp-th">
          <span class="c-drag" />
          <span class="c-no">#</span>
          <span class="c-tool">工具</span>
          <span class="c-plat">平台</span>
          <span class="c-users">用户</span>
          <span class="c-dec">决策</span>
          <span class="c-en">启用</span>
          <span class="c-ops">操作</span>
        </div>
        <div
          v-for="(r, i) in rules"
          :key="r.id"
          class="tp-tr tp-row"
          :class="{
            'is-off': !r.enabled,
            'is-deny': r.decision === 'deny' && r.enabled,
            'is-dragging': dragIndex === i,
            'is-over': dragOverIndex === i && dragIndex !== i
          }"
          :draggable="dragArmed === i"
          @dragstart="onDragStart(i, $event)"
          @dragover="onDragOver(i, $event)"
          @drop="onDrop(i, $event)"
          @dragend="resetDrag"
        >
          <span
            class="c-drag drag-handle"
            title="拖动调整优先级"
            @mousedown="onHandleDown(i)"
            @mouseup="onHandleUp"
          >
            <t-icon name="drag-move" />
          </span>

          <span class="c-no">
            <span class="no-badge">{{ i + 1 }}</span>
          </span>

          <span class="c-tool">
            <template v-if="r.tool === OUTBOUND_TOOL">
              <span class="tool-outbound" title="渠道发言开关（由上方「渠道发言」卡片管理）">
                <t-icon name="chat-bubble-1" /> 渠道发言
              </span>
            </template>
            <template v-else>
              <code class="tool-code">{{ r.tool || '*' }}</code>
              <span v-if="(r.tool || '*') === '*'" class="tool-meta">全部工具</span>
              <span v-else-if="r.tool.includes('*')" class="tool-meta">通配</span>
              <span v-else-if="basicToolNames.has(r.tool)" class="tool-meta tm-basic" title="基础工具默认已开放，此规则通常只在需要显式禁止时才必要">基础</span>
            </template>
          </span>

          <span class="c-plat">
            <span class="plat-tag" :class="`pf-${platClass(r.platform)}`">{{ r.platform || '*' }}</span>
          </span>

          <span class="c-users">
            <template v-if="isAllUsers(r.userIds)">
              <span class="user-all">全部用户</span>
            </template>
            <template v-else>
              <span v-for="u in r.userIds.slice(0, 3)" :key="u" class="user-chip">{{ u }}</span>
              <span v-if="r.userIds.length > 3" class="user-more" :title="r.userIds.join(', ')">
                +{{ r.userIds.length - 3 }}
              </span>
            </template>
          </span>

          <span class="c-dec">
            <span class="dec-tag" :class="r.decision === 'allow' ? 'dec-allow' : 'dec-deny'">
              <i class="dec-dot" />{{ r.decision === 'allow' ? '允许' : '禁止' }}
            </span>
          </span>

          <span class="c-en">
            <t-switch size="small" :value="r.enabled" @change="(v) => toggleEnabled(r, v)" />
          </span>

          <span class="c-ops">
            <button class="ic-btn" title="上移" :disabled="i === 0" @click="move(i, -1)">
              <t-icon name="arrow-up" />
            </button>
            <button class="ic-btn" title="下移" :disabled="i === rules.length - 1" @click="move(i, 1)">
              <t-icon name="arrow-down" />
            </button>
            <button class="ic-btn" title="编辑" @click="openEdit(r)">
              <t-icon name="edit-1" />
            </button>
            <button class="ic-btn ic-danger" title="删除" @click="confirmRemove(r)">
              <t-icon name="delete" />
            </button>
          </span>
        </div>
      </div>

      <!-- 空态 -->
      <div v-else class="tp-empty">
        <div class="em-icon"><t-icon name="lock-off" /></div>
        <div class="em-title">尚未配置任何规则</div>
        <div class="em-desc">当前所有平台下的工具均默认开放。添加规则后，被涉及平台的<b>敏感工具</b>会切换为白名单模式，基础工具（计算/文本/记忆）仍保持开放。</div>
        <t-button theme="primary" variant="outline" @click="openAdd">
          <template #icon><t-icon name="add" /></template>添加第一条规则
        </t-button>
      </div>
    </div>

    <!-- 添加 / 编辑规则弹窗 -->
    <t-dialog
      v-model:visible="dialogVisible"
      :header="editing ? '编辑规则' : '添加规则'"
      :width="620"
      class="tp-dialog"
      :confirm-btn="{ content: editing ? '保存' : '添加', loading: saving }"
      @confirm="confirmSave"
    >
      <t-form :data="form" label-align="top" class="tp-form">
        <t-form-item label="工具">
 <!-- 工具选择器：当前选择 + 通配快捷 + 搜索 + 分组列表 -->
       <div class="tool-picker">
    <!-- 当前选择态 -->
     <div class="tp-current">
       <span class="tpc-label">已选</span>
     <code class="tpc-value" :class="{ 'is-wild': form.tool.includes('*') }">{{ form.tool || '*' }}</code>
          <span class="tpc-desc">{{ currentToolDesc }}</span>
      </div>

   <!-- 通配快捷入口 -->
   <div class="tp-quick">
      <button
     v-for="q in wildcardPresets"
           :key="q.value"
     type="button"
     class="tq-btn"
 :class="{ 'is-active': form.tool === q.value }"
        :title="q.hint"
            @click="form.tool = q.value"
>{{ q.label }}</button>
        </div>

          <!-- 搜索框 -->
          <div class="tp-search-row">
            <t-input
              v-model="toolQuery"
              placeholder="搜索工具名或说明，也可直接输入自定义通配符"
              clearable
              class="tp-search"
            >
              <template #prefix-icon><t-icon name="search" /></template>
            </t-input>
            <label class="tp-only-sens" title="基础工具（计算/文本/记忆/只读查询）默认始终开放，通常无需为它们配规则">
              <t-switch v-model="onlySensitive" size="small" />
              <span>仅敏感</span>
            </label>
          </div>

  <!-- 自定义输入提示：搜索词是通配符且未匹配到工具时，允许直接采用 -->
         <div v-if="showCustomHint" class="tp-custom">
              <span>使用自定义模式 <code>{{ toolQuery.trim() }}</code></span>
  <t-button size="small" variant="outline" @click="applyCustomTool">采用</t-button>
            </div>

      <!-- 分组工具列表 -->
      <div class="tp-list">
              <div v-if="toolsLoading" class="tp-loading">
                <t-icon name="loading" class="spin" /> 正在加载工具…
   </div>
              <div v-else-if="!groupedTools.length" class="tp-none">
                <template v-if="onlySensitive">
                  没有匹配的敏感工具。可关闭「只看敏感工具」查看基础工具，或直接输入通配符。
                </template>
                <template v-else>
                  没有匹配的工具。可直接在上方输入通配符，或清空搜索词。
                </template>
              </div>
        <template v-else>
            <div v-for="g in groupedTools" :key="g.name" class="tp-group">
        <div class="tg-head">
              <span class="tg-name">{{ g.name }}</span>
                <span class="tg-count">{{ g.items.length }}</span>
       <button
        type="button"
 class="tg-wild"
       :title="`选择该分组的通配模式 ${g.wildcard}`"
         v-if="g.wildcard"
     @click="form.tool = g.wildcard"
           >{{ g.wildcard }}</button>
          </div>
         <div class="tg-items">
                <button
                  v-for="t in g.items"
                  :key="t.name"
                  type="button"
                  class="ti-card"
                  :class="{'is-active': form.tool === t.name, 'is-basic': t.risk === 'basic' }"
                  @click="form.tool = t.name"
                >
                  <span class="ti-top">
                    <span class="ti-name">{{ t.name }}</span>
                    <span v-if="t.risk === 'basic'" class="ti-risk rk-basic" title="基础工具：无对外副作用，默认始终开放">基础</span>
                    <span v-else class="ti-risk rk-sens" title="敏感工具：有实际危害面，受权限规则约束">敏感</span>
                  </span>
                  <span class="ti-desc">{{ t.description }}</span>
                  <t-icon v-if="form.tool === t.name" name="check" class="ti-check" />
                </button>
         </div>
      </div>
 </template>
          </div>
       </div>
          <div class="field-hint">
 点选工具，或使用 <code>*</code> 通配（如 <code>sandbox_*</code> 匹配所有沙箱工具）。
        </div>
   </t-form-item>

        <t-form-item label="平台">
          <t-select v-model="form.platform" filterable creatable placeholder="选择或输入平台" :options="platformOptions" />
          <div class="field-hint">常见值：web / telegram / misskey。<code>*</code> 表示所有平台。</div>
        </t-form-item>

        <t-form-item label="用户">
          <t-input v-model="userIdsText" placeholder="逗号分隔，如 u1,u2；* 表示全部用户" />
          <div class="field-hint">多个用户用英文逗号分隔；留空或 <code>*</code> 表示全部用户。</div>
        </t-form-item>

        <t-form-item label="决策">
          <t-radio-group v-model="form.decision" variant="default-filled">
            <t-radio-button value="allow"><span class="dot dot-allow" />允许</t-radio-button>
            <t-radio-button value="deny"><span class="dot dot-deny" />禁止</t-radio-button>
          </t-radio-group>
        </t-form-item>

        <div class="form-row">
          <t-form-item label="排序" class="fr-item">
            <t-input-number v-model="form.sort" :min="-1000" :max="1000" style="width: 140px" />
            <div class="field-hint">数字越小越先评估。</div>
          </t-form-item>
          <t-form-item label="启用" class="fr-item">
            <t-switch v-model="form.enabled" />
            <div class="field-hint">禁用后该规则不参与评估。</div>
          </t-form-item>
        </div>

        <!-- 效果预览 -->
        <div class="preview" :class="form.decision === 'allow' ? 'pv-allow' : 'pv-deny'">
          <t-icon :name="form.decision === 'allow' ? 'check-circle' : 'close-circle'" />
          <span>
            在
            <b>{{ form.platform === '*' ? '所有平台' : form.platform || '所有平台' }}</b>
            上，对
            <b>{{ previewUsers }}</b>
            <b class="pv-strong">{{ form.decision === 'allow' ? '开放' : '禁止' }}</b>
            <b>{{ previewTool }}</b>
          </span>
        </div>
      </t-form>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botToolPermApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const rules = ref([])
const loading = ref(false)
const saving = ref(false)
const resetting = ref(false)

// 已知平台（用于概览与下拉）；后端不限制取值，故下拉可自由输入
const KNOWN_PLATFORMS = ['web', 'telegram', 'misskey']

// 后端用这个保留工具名把「渠道发言」建模成一条可授权能力（见 toolperm/outbound.go）。
// 它不是真实工具，在规则列表里需要特殊渲染，也不应出现在工具选择器中。
const OUTBOUND_TOOL = '__outbound_reply'
const platformOptions = [
  { label: '全部平台 (*)', value: '*' },
  { label: 'web', value: 'web' },
  { label: 'telegram', value: 'telegram' },
  { label: 'misskey', value: 'misskey' }
]

function platClass(p) {
  const v = p || '*'
  if (v === '*') return 'all'
  return KNOWN_PLATFORMS.includes(v) ? v : 'other'
}

// 平台覆盖概览：与后端 platformHasEnabledRule 语义保持一致
// —— 平台被任意启用规则覆盖（精确匹配或 platform=*）即进入「受限」白名单模式。
// 出站规则（__outbound_reply）不算工具管控，需排除，否则开了「只看不发」
// 会让所有平台显示成「管控中」，与工具权限的实际状态不符。
const platformSummary = computed(() => {
  const active = rules.value.filter(r => r.enabled && r.tool !== OUTBOUND_TOOL)
  const names = new Set(KNOWN_PLATFORMS)
  active.forEach(r => { if (r.platform && r.platform !== '*') names.add(r.platform) })
  return [...names].map(name => {
    const hits = active.filter(r => (r.platform || '*') === '*' || r.platform === name)
    return { name, constrained: hits.length > 0, count: hits.length }
  })
})

function isAllUsers(ids) {
  return !ids || ids.length === 0 || ids.includes('*')
}
function usersToText(ids) {
  if (isAllUsers(ids)) return '*'
  return ids.join(',')
}
function textToUsers(text) {
  const t = (text || '').trim()
  if (!t || t === '*') return ['*']
  return t.split(',').map(s => s.trim()).filter(Boolean)
}

const previewTool = computed(() => {
  const t = (form.value.tool || '*').trim()
  return t === '*' ? '全部工具' : `工具 ${t}`
})
const previewUsers = computed(() => {
  const list = textToUsers(userIdsText.value)
  if (list.includes('*')) return '全部用户'
  return list.length === 1 ? `用户 ${list[0]}` : `${list.length} 个指定用户`
})

async function load() {
  loading.value = true
  try {
    rules.value = await botToolPermApi.list(props.botId)
  } catch (e) {
    MessagePlugin.error('加载工具权限失败：' + (e.message || e))
  } finally {
    loading.value = false
  }
}
onMounted(() => {
  load()
  loadTools() // 规则列表需要 risk 信息来标注基础工具
  loadOutbound()
})

// ---- 渠道发言（只看不发）----
// 与工具权限分开：Bot 的自动回复走Pipeline Action → Channel.Send，
// 完全不经过ToolManager，所以禁用发言类工具拦不住「被 @ 后自动回帖」。
const outbound = ref([])
const obLoading = ref(false)
const obSaving = ref('')

async function loadOutbound() {
  obLoading.value = true
  try {
    outbound.value = await botToolPermApi.getOutbound(props.botId)
  } catch (e) {
    console.warn('[toolperm] load outbound failed:', e)
    outbound.value = []
  } finally {
    obLoading.value = false
  }
}

// v 是开关值（true=可发言），readOnly 与之相反
async function toggleOutbound(o, v) {
  const readOnly = !v
  obSaving.value = o.platform
  try {
    await botToolPermApi.setOutbound(props.botId, o.platform, readOnly)
    o.readOnly = readOnly
    MessagePlugin.success(readOnly? `已关闭 ${o.platform} 的发言` : `已恢复 ${o.platform} 的发言`)
    // 「全部渠道」与单渠道规则是两条独立规则，改动后重新拉取以反映真实合并结果
    if (o.platform === '*') await loadOutbound()
    // 出站规则也存在规则表里，同步刷新列表
    await load()
  } catch (e) {
    MessagePlugin.error('设置失败：' + (e.message || e))
  } finally {
    obSaving.value = ''
  }
}

async function toggleEnabled(r, v) {
  r.enabled = v
  try {
    await botToolPermApi.update(props.botId, r.id, { ...toPayload(r), enabled: v })
    MessagePlugin.success(v ? '规则已启用' : '规则已禁用')
  } catch (e) {
    r.enabled = !v
    MessagePlugin.error('操作失败：' + (e.message || e))
  }
}

// 后端 applyCommon 会把请求里缺失的 tool/platform/userIds/decision 归一化为默认值，
// 因此局部更新（如仅改enabled/sort）也必须回传完整字段，否则规则会被悄悄重置为
// tool=* platform=* userIds=["*"] decision=allow。
function toPayload(r) {
  return {
    tool: r.tool || '*',
    platform: r.platform || '*',
    userIds: isAllUsers(r.userIds) ? ['*'] : r.userIds,
    decision: r.decision === 'deny' ? 'deny' : 'allow',
    enabled: !!r.enabled,
    sort: Number(r.sort || 0)
  }
}

async function move(i, d) {
  const j = i + d
  if (j < 0 || j >= rules.value.length) return
  const next = rules.value.slice()
  ;[next[i], next[j]] = [next[j], next[i]]
  await applyOrder(next)
}

// ---- 拖拽排序 ----
// 用原生 HTML5 拖放（项目未引入 sortablejs / vuedraggable，不为此加依赖）。
// 仅允许从手柄发起拖拽：整行常驻 draggable 会干扰文本选中与行内按钮点击，
// 因此按下手柄时才把该行标记为可拖。
const dragIndex = ref(-1)   // 正在拖拽的行
const dragOverIndex = ref(-1) // 当前悬停的落点
const dragArmed = ref(-1)   // 已按下手柄、允许拖拽的行

function onHandleDown(i) { dragArmed.value = i }
function onHandleUp() { if (dragIndex.value === -1) dragArmed.value = -1 }

function onDragStart(i, e) {
  if (dragArmed.value !== i) { e.preventDefault(); return }
  dragIndex.value = i
  e.dataTransfer.effectAllowed = 'move'
  // Firefox 需要写入数据才会触发后续 drag 事件
  e.dataTransfer.setData('text/plain', String(i))
}

function onDragOver(i, e) {
  if (dragIndex.value === -1) return
  e.preventDefault()
  e.dataTransfer.dropEffect = 'move'
  dragOverIndex.value = i
}

async function onDrop(i, e) {
  e.preventDefault()
  const from = dragIndex.value
  resetDrag()
  if (from === -1 || from === i) return
  const next = rules.value.slice()
  const [moved] = next.splice(from, 1)
  next.splice(i, 0, moved)
  await applyOrder(next)
}

function resetDrag() {
  dragIndex.value = -1
  dragOverIndex.value = -1
  dragArmed.value = -1
}

// applyOrder 接收新顺序，按位置重排 sort 为 0..n-1 并只持久化变更的规则。
// 拖拽可跨越多个位置，无法沿用「交换两条 sort」的做法，必须整体重编号；
// 顺带把历史遗留的重复 / 空洞 sort 一并规整。
async function applyOrder(next) {
  const changed = []
  next.forEach((r, idx) => {
    if (Number(r.sort) !== idx) {
      r.sort = idx
      changed.push(r)
    }
  })
  rules.value = next
  if (!changed.length) return
  try {
    await Promise.all(changed.map(r => botToolPermApi.update(props.botId, r.id, toPayload(r))))
  } catch (e) {
    MessagePlugin.error('排序保存失败：' + (e.message || e))
    await load() // 回滚到服务端真实顺序
  }
}

function confirmRemove(r) {
  const dlg = DialogPlugin.confirm({
    header: '删除规则',
    theme: 'warning',
    body: `确认删除规则「${r.tool || '*'} @ ${r.platform || '*'}」？删除后若该平台再无任何规则，其工具将恢复为全部开放。`,
    onConfirm: async () => {
      try {
        await botToolPermApi.remove(props.botId, r.id)
        rules.value = rules.value.filter(x => x.id !== r.id)
        MessagePlugin.success('规则已删除')
      } catch (e) {
        MessagePlugin.error('删除失败：' + (e.message || e))
      }
      dlg.destroy()
    }
  })
}

function confirmReset() {
  const dlg = DialogPlugin.confirm({
    header: '恢复默认',
    theme: 'warning',
    body: '将清除该 Bot 的全部自定义规则，仅保留 web 的「全部开放」基线规则。此操作不可撤销。',
    onConfirm: async () => {
      resetting.value = true
      try {
        await botToolPermApi.resetDefaults(props.botId)
        await load()
        MessagePlugin.success('已恢复默认')
      } catch (e) {
        MessagePlugin.error('恢复默认失败：' + (e.message || e))
      } finally {
        resetting.value = false
      }
      dlg.destroy()
    }
  })
}

const dialogVisible = ref(false)
const editing = ref(false)
const editingId = ref(null)
const userIdsText = ref('*')
const form = ref({ tool: '*', platform: 'web', decision: 'allow', enabled: true, sort: 0 })

// ---- 工具选择器 ----
// 用分组卡片列表代替下拉自动补全：工具数量多、名字长、说明需要换行，
// 窄下拉框里 "name — desc" 单行会挤成一团（曾出现文字重叠），
// 且用户无法概览「系统里到底有哪些工具」。
const allTools = ref([])     // { name, description, category, risk }[]
const toolsLoading = ref(false)
const toolQuery = ref('')
// 默认只看敏感工具：基础工具始终开放，管理员基本不需要为它们配规则
const onlySensitive = ref(true)

// 通配快捷入口：覆盖最常用的几种批量授权意图
const wildcardPresets = [
  { label: '全部工具 *', value: '*', hint: '匹配系统中的所有工具' },
  { label: 'sandbox_*', value: 'sandbox_*', hint: '所有沙箱与文件工具' },
  { label: 'web_*', value: 'web_*', hint: '所有联网工具（搜索、抓取）' },
  { label: 'task_*', value: 'task_*', hint: '所有任务/工作流工具' },
  { label: 'text_*', value: 'text_*', hint: '所有文本处理工具' }
]

// 当前选中工具的说明文案
const currentToolDesc = computed(() => {
  const v = (form.value.tool || '*').trim()
  if (v === '*') return '匹配系统中的所有工具（含基础工具）'
  if (v.includes('*')) {
    const n = allTools.value.filter(t => matchWildcard(t.name, v)).length
    return `通配模式 · 当前匹配 ${n} 个工具`
  }
  const hit = allTools.value.find(t => t.name === v)
  if (!hit) return '自定义工具名（不在当前已注册列表中）'
  return hit.risk === 'basic'
    ? `${hit.description} · 基础工具，默认已开放`
    : hit.description
})

// 与后端 matchGlob 对齐的前缀/后缀/中缀通配匹配，仅用于展示匹配数量
function matchWildcard(name, pattern) {
  if (pattern === '*') return true
  if (!pattern.includes('*')) return name === pattern
  const parts = pattern.split('*')
  if (parts.length === 2) {
    const [pre, suf] = parts
    return name.startsWith(pre) && name.endsWith(suf) && name.length >= pre.length + suf.length
  }
  return name.includes(pattern.replace(/\*/g, ''))
}

// 按 category 分组 + 搜索过滤 + 风险筛选
// 敏感工具分组排在前面：管理员配规则的目标基本都是敏感工具。
const groupedTools = computed(() => {
  const q = toolQuery.value.trim().toLowerCase()
  let filtered = allTools.value
  if (onlySensitive.value) {
    filtered = filtered.filter(t => t.risk !== 'basic')
  }
  if (q) {
    filtered = filtered.filter(t =>
      t.name.toLowerCase().includes(q) ||
      (t.description || '').toLowerCase().includes(q))
  }

  const map = new Map()
  for (const t of filtered) {
    const cat = t.category || '其他'
    if (!map.has(cat)) map.set(cat, [])
    map.get(cat).push(t)
  }
  const groups = [...map.entries()].map(([name, items]) => ({
    name,
    items,
    wildcard: groupWildcard(items),
    // 组内只要有敏感工具就算敏感组，用于排序
    sensitive: items.some(t => t.risk !== 'basic')
  }))
  // 敏感组优先
  groups.sort((a, b) => Number(b.sensitive) - Number(a.sensitive))
  return groups
})

// 若分组内工具共享同一 "xxx_" 前缀，给出该分组的通配建议
function groupWildcard(items) {
  if (items.length < 2) return ''
  const first = items[0].name
  const idx = first.indexOf('_')
  if (idx <= 0) return ''
  const prefix = first.slice(0, idx + 1)
  return items.every(t => t.name.startsWith(prefix)) ? `${prefix}*` : ''
}

// 搜索词本身是通配符且没匹配到具体工具时，提供「采用自定义模式」入口
const showCustomHint = computed(() => {
  const q = toolQuery.value.trim()
  return q.includes('*') && q !== form.value.tool
})
function applyCustomTool() {
  form.value.tool = toolQuery.value.trim()
}

async function loadTools() {
  toolsLoading.value = true
  try {
    allTools.value = await botToolPermApi.listTools(props.botId)
  } catch (e) {
    // 静默失败：仍可通过搜索框输入自定义工具名，不阻断核心功能
    console.warn('[toolperm] load tools failed:', e)
    allTools.value = []
  } finally {
    toolsLoading.value = false
  }
}

// 基础工具名集合，用于在规则列表里标注「该规则针对的是默认已开放的基础工具」
const basicToolNames = computed(
  () => new Set(allTools.value.filter(t => t.risk === 'basic').map(t => t.name))
)

function openAdd() {
  editing.value = false
  editingId.value = null
  const maxSort = rules.value.reduce((m, r) => Math.max(m, Number(r.sort || 0)), -1)
  form.value = { tool: '*', platform: 'web', decision: 'allow', enabled: true, sort: maxSort + 1 }
  userIdsText.value = '*'
  toolQuery.value = ''
  dialogVisible.value = true
  loadTools()
}
function openEdit(r) {
  editing.value = true
  editingId.value = r.id
  form.value = {
    tool: r.tool || '*',
    platform: r.platform || '*',
    decision: r.decision || 'allow',
    enabled: !!r.enabled,
    sort: Number(r.sort || 0)
  }
  userIdsText.value = usersToText(r.userIds)
  toolQuery.value = ''
  dialogVisible.value = true
  loadTools()
}

async function confirmSave() {
  const payload = {
    tool: (form.value.tool || '').trim() || '*',
    platform: (form.value.platform || '').trim() || '*',
    userIds: textToUsers(userIdsText.value),
    decision: form.value.decision === 'deny' ? 'deny' : 'allow',
    enabled: form.value.enabled,
    sort: Number(form.value.sort || 0)
  }
  saving.value = true
  try {
    if (editing.value) {
      const updated = await botToolPermApi.update(props.botId, editingId.value, payload)
      const idx = rules.value.findIndex(x => x.id === editingId.value)
      if (idx >= 0 && updated) rules.value[idx] = updated
      MessagePlugin.success('已保存')
    } else {
      const created = await botToolPermApi.create(props.botId, payload)
      if (created) rules.value.push(created)
      MessagePlugin.success('规则已添加')
    }
    // 服务端按 sort 排序，重新拉取保证顺序与后端评估顺序一致
    dialogVisible.value = false
    await load()
  } catch (e) {
    MessagePlugin.error('保存失败：' + (e.message || e))
  } finally {
    saving.value = false
  }
}
</script>

<style scoped>
.tp-wrap { max-width: 1000px; }

/* ---- 页头 ---- */
.tp-head { margin-bottom: 18px; }
.tp-title { font-size: 16px; font-weight: 600; margin: 0 0 6px; color: #1d1d1f; }
.tp-desc { font-size: 13px; color: #888; margin: 0; line-height: 1.6; }

/* ---- 卡片 ---- */
.tp-card {
  border: 1px solid #ececec; border-radius: 12px; padding: 18px 20px;
  background: #fff; margin-bottom: 16px;
}
.card-title { font-size: 14px; font-weight: 600; color: #1d1d1f; display: flex; align-items: center; gap: 8px; }
.card-sub { font-size: 12px; color: #999; margin-top: 4px; line-height: 1.6; }
.count-pill {
  font-size: 11px; font-weight: 600; color: #666; background: #f0f1f3;
  border-radius: 10px; padding: 1px 8px;
}

/* ---- 判定方式说明 ---- */
.tp-explain { background: #fcfcfd; }
.ex-grid {
  margin-top: 14px; display: grid; gap: 12px;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
}
.ex-item { display: flex; gap: 10px; align-items: flex-start; }
.ex-badge {
  width: 26px; height: 26px; border-radius: 8px; flex-shrink: 0;
  display: flex; align-items: center; justify-content: center; font-size: 15px;
}
.ex-open  { background: #e8f8ee; color: #21935a; }
.ex-lock  { background: #fdecec; color: #d54941; }
.ex-order { background: #eef1fb; color: #4b62c9; }
.ex-star  { background: #f3f0fb; color: #7a5bc7; }
.ex-body { min-width: 0; }
.ex-h { font-size: 13px; font-weight: 600; color: #333; }
.ex-t { font-size: 12px; color: #8a8a8e; line-height: 1.6; margin-top: 2px; }
.ex-t code, .ex-h code {
  background: #f1f2f4; padding: 1px 5px; border-radius: 4px; font-size: 11.5px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}
.ex-note {
  margin-top: 14px; padding: 9px 12px; border-radius: 8px; background: #f7f8fa;
  font-size: 12px; color: #7a7a7e; display: flex; align-items: center; gap: 7px;
}

/* ---- 规则头部 ---- */
.tp-rule-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.tp-head-ops { display: flex; gap: 8px; flex-shrink: 0; }

/* ---- 平台覆盖概览 ---- */
.plat-summary { margin-top: 14px; display: flex; flex-wrap: wrap; gap: 8px; }
.ps-chip {
  display: inline-flex; align-items: center; gap: 6px; cursor: default;
  font-size: 12px; padding: 4px 10px; border-radius: 20px; border: 1px solid transparent;
}
.ps-chip em { font-style: normal; opacity: .75; font-size: 11px; }
.ps-dot { width: 6px; height: 6px; border-radius: 50%; flex-shrink: 0; }
.ps-open   { background: #f0faf4; border-color: #d6f0e2; color: #21935a; }
.ps-open   .ps-dot { background: #34c759; }
.ps-limited{ background: #fff8f0; border-color: #ffe4c4; color: #b96a1b; }
.ps-limited .ps-dot { background: #f5a623; }

/* ---- 骨架屏 ---- */
.tp-skeleton { margin-top: 16px; display: flex; flex-direction: column; gap: 8px; }
.sk-row {
  height: 46px; border-radius: 8px;
  background: linear-gradient(90deg, #f5f5f7 25%, #eeeef0 37%, #f5f5f7 63%);
  background-size: 400% 100%; animation: sk 1.3s ease infinite;
}
@keyframes sk { 0% { background-position: 100% 50% } 100% { background-position: 0 50% } }

/* ---- 表格 ---- */
.tp-table { margin-top: 16px; display: flex; flex-direction: column; gap: 6px; }
.tp-tr { display: flex; align-items: center; gap: 12px; padding: 11px 12px; border-radius: 8px; }
.tp-th {
  background: #fafafa; font-size: 11.5px; color: #999; font-weight: 600;
  letter-spacing: .02em; padding: 8px 12px; border: 1px solid #f0f0f0;
}
.tp-row { border: 1px solid #f0f0f0; transition: background .15s, border-color .15s, opacity .15s; }
.tp-row:hover { background: #fcfcfd; border-color: #e4e4e6; }
.tp-row.is-deny { border-left: 3px solid #f5c2c0; }
.tp-row.is-off { opacity: .5; }
.tp-row.is-off .tool-code { text-decoration: line-through; }
/* 拖拽中：源行淡出，落点行顶部显示插入指示线 */
.tp-row.is-dragging { opacity: .35; border-style: dashed; }
.tp-row.is-over { border-top: 2px solid #4b62c9; background: #f7f8fd; }

.c-drag { width: 18px; flex-shrink: 0; display: flex; justify-content: center; }
.drag-handle {
  color: #d0d0d4; cursor: grab; font-size: 14px; border-radius: 4px;
  transition: color .15s, background .15s;
}
.drag-handle:hover { color: #888; background: #f0f1f3; }
.drag-handle:active { cursor: grabbing; }

.c-no { width: 26px; flex-shrink: 0; display: flex; justify-content: center; }
.no-badge {
  min-width: 20px; height: 20px; border-radius: 6px; background: #f2f3f5; color: #888;
  font-size: 11.5px; font-weight: 600; display: inline-flex; align-items: center; justify-content: center;
}
.tp-th .c-no { color: #999; }

.c-tool { flex: 1.5; min-width: 0; display: flex; align-items: center; gap: 8px; }
.tool-code {
  background: #f1f2f4; padding: 3px 8px; border-radius: 5px; font-size: 12px; color: #333;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 100%;
}
.tool-meta { font-size: 11px; color: #b0b0b4; flex-shrink: 0; }
.tm-basic {
  background: #edf6ee; color: #4a9c3f; padding: 1px 6px; border-radius: 4px; font-size: 10px;
}

.c-plat { width: 100px; flex-shrink: 0; }
.plat-tag {
  display: inline-block; font-size: 11.5px; padding: 2px 9px; border-radius: 5px; font-weight: 500;
}
.pf-web      { background: #eef1fb; color: #4b62c9; }
.pf-telegram { background: #e8f4fd; color: #2b7fc4; }
.pf-misskey  { background: #eef8ec; color: #4a9c3f; }
.pf-all      { background: #f3f0fb; color: #7a5bc7; }
.pf-other    { background: #f2f3f5; color: #777; }

.c-users { flex: 1.4; min-width: 0; display: flex; align-items: center; gap: 4px; flex-wrap: nowrap; overflow: hidden; }
.user-all { font-size: 12px; color: #999; }
.user-chip {
  font-size: 11.5px; background: #f5f5f7; color: #555; padding: 2px 7px; border-radius: 4px;
  max-width: 90px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.user-more { font-size: 11px; color: #aaa; cursor: default; flex-shrink: 0; }

.c-dec { width: 74px; flex-shrink: 0; }
.dec-tag {
  display: inline-flex; align-items: center; gap: 5px;
  font-size: 12px; padding: 2px 9px; border-radius: 5px; font-weight: 500;
}
.dec-dot { width: 6px; height: 6px; border-radius: 50%; }
.dec-allow { background: #edf8f1; color: #21935a; }
.dec-allow .dec-dot { background: #34c759; }
.dec-deny  { background: #fdeeed; color: #d54941; }
.dec-deny  .dec-dot { background: #e5544b; }

.c-en { width: 50px; flex-shrink: 0; }

.c-ops { display: flex; gap: 2px; flex-shrink: 0; width: 128px; justify-content: flex-end; }
.ic-btn {
  width: 28px; height: 28px; border: none; background: transparent; border-radius: 6px;
  color: #999; cursor: pointer; display: inline-flex; align-items: center; justify-content: center;
  font-size: 14px; transition: background .15s, color .15s;
}
.ic-btn:hover:not(:disabled) { background: #f0f1f3; color: #333; }
.ic-btn:disabled { opacity: .3; cursor: not-allowed; }
.ic-danger:hover:not(:disabled) { background: #fdeeed; color: #d54941; }

/* ---- 空态 ---- */
.tp-empty { padding: 42px 20px; text-align: center; }
.em-icon {
  width: 46px; height: 46px; border-radius: 14px; background: #f0faf4; color: #34c759;
  font-size: 22px; display: inline-flex; align-items: center; justify-content: center; margin-bottom: 14px;
}
.em-title { font-size: 14px; font-weight: 600; color: #444; }
.em-desc { font-size: 12.5px; color: #999; margin: 6px auto 18px; max-width: 420px; line-height: 1.7; }

/* ---- 渠道发言（只看不发）---- */
.ob-badge {
  font-size: 10.5px; font-weight: 500; color: #a86a12;
  background: #fff6e6; border: 1px solid #ffe4bf;
  border-radius: 4px; padding: 1px 6px;
}
.ob-loading { padding: 18px 2px; font-size: 12.5px; color: #aaa; }
.ob-grid {
  margin-top: 14px; display: grid; gap: 8px;
  grid-template-columns: repeat(auto-fit, minmax(190px, 1fr));
}
.ob-item {
  display: flex; align-items: center; justify-content: space-between; gap: 10px;
  padding: 9px 12px; border: 1px solid #ececef; border-radius: 8px; background: #fff;
  transition: border-color .15s, background .15s;
}
.ob-item.is-mute { border-color: #f3d3d0; background: #fffafa; }
.ob-left { display: flex; align-items: center; gap: 8px; min-width: 0; }
.ob-plat {
  font-size: 11.5px; padding: 2px 9px; border-radius: 5px; font-weight: 500; flex-shrink: 0;
}
.ob-state { font-size: 11.5px; color: #9a9aa0; }
.ob-item.is-mute .ob-state { color: #c8443c; }
.ob-note {
  margin-top: 12px; padding: 9px 12px; border-radius: 8px; background: #f7f8fa;
  font-size: 11.5px; color: #7a7a7e; display: flex; align-items: flex-start; gap: 7px;
  line-height: 1.6;
}
.ob-note code {
  background: #f1f2f4; padding: 0 4px; border-radius: 3px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}

/* 规则列表里的出站规则标记 */
.tool-outbound {
  display: inline-flex; align-items: center; gap: 5px;
  font-size: 12px; color: #a86a12; background: #fff6e6;
  border: 1px solid #ffe4bf; border-radius: 5px; padding: 2px 8px;
}

/* ---- 弹窗表单 ---- */
.tp-form { padding-top: 4px; }

/* TDesign 的 .t-form__controls-content 默认是 flex row，
   而我们把 .field-hint 作为控件的兄弟节点放在同一个 controls 里，
   会被排到控件右侧并溢出弹窗（曾出现提示文字跑到弹窗外的布局 bug）。
   这里强制纵向堆叠，让提示文字老老实实待在控件下方。 */
.tp-form :deep(.t-form__controls-content) {
  flex-direction: column;
  align-items: stretch;
  gap: 0;
}
/* 控件本身占满可用宽度 */
.tp-form :deep(.t-form__controls-content > .t-input),
.tp-form :deep(.t-form__controls-content > .t-select),
.tp-form :deep(.t-form__controls-content > .t-select-input) {
  width: 100%;
}

.field-hint { font-size: 11.5px; color: #aaa; margin-top: 5px; line-height: 1.5; }
.field-hint code {
  background: #f1f2f4; padding: 0 4px; border-radius: 3px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
}
.form-row { display: flex; gap: 28px; }
.fr-item { flex: 0 0 auto; }
.dot { display: inline-block; width: 7px; height: 7px; border-radius: 50%; margin-right: 6px; vertical-align: middle; }
.dot-allow { background: #34c759; }
.dot-deny { background: #e5544b; }

/* ---- 工具选择器 ---- */
.tool-picker {
  border: 1px solid #e7e7ea; border-radius: 10px; background: #fcfcfd; overflow: hidden;
}

/* 当前选择态 */
.tp-current {
  display: flex; align-items: center; gap: 8px; flex-wrap: wrap;
  padding: 10px 12px; background: #f4f6fb; border-bottom: 1px solid #e9ebf2;
}
.tpc-label { font-size: 11px; color: #8a8a8e; flex-shrink: 0; }
.tpc-value {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12.5px; font-weight: 600; color: #3b4ea8;
  background: #e6ebfa; padding: 3px 9px; border-radius: 5px; flex-shrink: 0;
}
.tpc-value.is-wild { color: #7a5bc7; background: #f0eafb; }
.tpc-desc { font-size: 11.5px; color: #9a9a9e; line-height: 1.5; min-width: 0; }

/* 通配快捷入口 */
.tp-quick {
  display: flex; flex-wrap: wrap; gap: 6px; padding: 10px 12px 0;
}
.tq-btn {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 11.5px; padding: 3px 9px; border-radius: 20px; cursor: pointer;
  border: 1px solid #e2e3e8; background: #fff; color: #6a6a70;
  transition: border-color .15s, color .15s, background .15s;
}
.tq-btn:hover { border-color: #b9c2e6; color: #4b62c9; }
.tq-btn.is-active { background: #eef1fb; border-color: #9daaea; color: #3b4ea8; font-weight: 600; }

/* 搜索行 */
.tp-search-row { display: flex; align-items: center; gap: 10px; margin: 10px 12px; }
.tp-search { flex: 1; min-width: 0; }
.tp-only-sens {
  display: flex; align-items: center; gap: 6px; flex-shrink: 0;
  font-size: 11.5px; color: #7a7a80; cursor: pointer; user-select: none;
}

/* 自定义模式提示 */
.tp-custom {
  margin: 0 12px 10px; padding: 7px 11px; border-radius: 7px;
  background: #fffaf0; border: 1px solid #ffe9c9;
  display: flex; align-items: center; justify-content: space-between; gap: 10px;
  font-size: 12px; color: #a86a12;
}
.tp-custom code {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  background: #fff1d9; padding: 1px 6px; border-radius: 4px; font-weight: 600;
}

/* 分组列表：固定高度 + 滚动，避免弹窗被撑爆 */
.tp-list {
  max-height: 300px; overflow-y: auto; padding: 4px 12px 12px;
  border-top: 1px solid #f0f0f2; background: #fff;
  scrollbar-width: thin;
}
.tp-list::-webkit-scrollbar { width: 8px; }
.tp-list::-webkit-scrollbar-thumb { background: #dcdce0; border-radius: 4px; }
.tp-list::-webkit-scrollbar-thumb:hover { background: #c8c8cd; }
.tp-loading, .tp-none {
  padding: 24px 4px; text-align: center; font-size: 12.5px; color: #aaa; line-height: 1.7;
}
.spin { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg) } }

.tp-group { margin-top: 10px; }
.tp-group:first-child { margin-top: 4px; }
/* 分组标题吸顶：长列表滚动时始终知道当前在看哪个分类 */
.tg-head {
  display: flex; align-items: center; gap: 7px; margin-bottom: 6px;
  position: sticky; top: 0; z-index: 1;
  background: #fff; padding: 4px 0;
}
.tg-name { font-size: 12px; font-weight: 600; color: #55555a; }
.tg-count {
  font-size: 10.5px; color: #a0a0a6; background: #f0f1f3;
  border-radius: 8px; padding: 0 6px; line-height: 16px;
}
.tg-wild {
  margin-left: auto; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 11px; color: #8a7bc0; background: #f5f2fc; border: 1px solid #e6dff7;
  border-radius: 5px; padding: 1px 7px; cursor: pointer; transition: background .15s;
}
.tg-wild:hover { background: #ece5fa; }

/* 工具卡片：单列列表。
   两列网格会把 task_control / web_fetch 这类稍长的工具名挤成 "task_co…"——
   权限配置界面看不清工具全名是不可接受的。单列下 name 有足够宽度完整显示，
   desc 跟在右侧同一行，卡片高度也天然统一。 */
.tg-items { display: flex; flex-direction: column; gap: 5px; }
.ti-card {
  position: relative; text-align: left; cursor: pointer; width: 100%;
  display: flex; align-items: baseline; gap: 10px;
  padding: 8px 30px 8px 11px; border-radius: 7px;
  border: 1px solid #ececef; background: #fff;
  transition: border-color .15s, background .15s, box-shadow .15s;
}
.ti-card:hover { border-color: #c3cbe9; background: #fafbff; }
.ti-card.is-active {
  border-color: #7d8fe0; background: #f2f5fe;
  box-shadow: 0 0 0 2px rgba(125, 143, 224, .14);
}
.ti-name {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px; font-weight: 600; color: #3a3a40; line-height: 1.5;
  white-space: nowrap; /* 单列有足够宽度，不再截断 */
}
.ti-card.is-active .ti-name { color: #3b4ea8; }

/* 风险标记 */
.ti-top { display: flex; align-items: center; gap: 6px; flex-shrink: 0; }
.ti-risk {
  flex-shrink: 0; font-size: 10px; line-height: 15px; padding: 0 5px;
  border-radius: 4px; font-weight: 500;
}
.rk-basic { background: #edf6ee; color: #4a9c3f; }
.rk-sens  { background: #fdf0ec; color: #cf6a3f; }
/* 基础工具整体降饱和度：它们默认就是开放的，不是配置重点 */
.ti-card.is-basic { background: #fcfcfc; }
.ti-card.is-basic .ti-name { color: #6a6a70; }

/* desc 跟在名字右侧，单行省略 */
.ti-desc {
  font-size: 11px; color: #9c9ca2; line-height: 1.5; min-width: 0;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.ti-check {
  position: absolute; right: 9px; top: 50%; transform: translateY(-50%);
  color: #4b62c9; font-size: 13px;
}

.preview {
  margin-top: 6px; padding: 11px 13px; border-radius: 9px; font-size: 12.5px;
  display: flex; align-items: center; gap: 8px; line-height: 1.6;
}
.preview b { font-weight: 600; }
.pv-strong { padding: 0 2px; }
.pv-allow { background: #f0faf4; color: #1e8a54; }
.pv-deny { background: #fdf1f0; color: #c8443c; }
</style>

<!-- t-dialog 的 body 挂在组件外层，scoped 选择器命中不了，故用全局样式。
     规则弹窗内容较高（工具选择面板+ 5 个字段），必须限制高度并让 body 内部滚动，
     否则在笔记本屏幕上底部的「决策 / 排序 / 预览」会被视口截断、按钮点不到。 -->
<style>
.tp-dialog .t-dialog__body {
  max-height: calc(90vh - 180px);
  overflow-y: auto;
}
</style>
