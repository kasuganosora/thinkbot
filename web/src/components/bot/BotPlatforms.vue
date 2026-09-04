<template>
  <div class="plat-wrap" data-testid="bot-platform">
    <!-- 左：平台列表 -->
    <div class="plat-list">
      <div
        v-for="p in list"
        :key="p.id"
        class="plat-item"
        :class="{ active: cur && cur.id === p.id }"
        @click="select(p)"
      >
        <div class="pi-icon" :style="iconTint(metaOf(p.type)?.color)">{{ iconText(metaOf(p.type), p.name) }}</div>
        <div class="pi-main">
          <div class="pi-name">{{ p.name }}</div>
          <div class="pi-sub" :class="{ on: p.enabled, off: p.configured && !p.enabled }">{{ statusText(p) }}</div>
        </div>
      </div>
      <button class="plat-add" data-testid="platform-add" @click="openAdd">
        <t-icon name="add" /> 添加平台
      </button>
    </div>

    <!-- 右：详情 -->
    <div v-if="cur" class="plat-detail">
      <div class="pd-head">
        <div class="pd-avatar" :style="iconTint(curMeta?.color)">{{ iconText(curMeta, cur.name) }}</div>
        <div class="pd-title">
          <div class="pd-name">{{ cur.name }}</div>
          <div class="pd-id">平台标识：{{ cur.type }}</div>
        </div>
      </div>

      <div class="pd-enable">
        <div>
          <div class="pd-enable-title">启用此平台</div>
          <div class="pd-enable-desc">关闭后 Bot 不会连接该平台，凭据与节奏配置仍保留</div>
        </div>
        <t-switch v-model="cur.enabled" size="large" data-testid="platform-enable" />
      </div>

      <h4 class="sec-title">凭据配置</h4>
      <div class="cred-form">
        <div v-for="f in fields" :key="f.key" class="cred-item" :class="{ 'cred-item-inline': isSwitch(f) }">
          <template v-if="isSwitch(f)">
            <div class="cred-switch-row">
              <div class="cred-switch-label">
                <label>{{ f.label }} <span v-if="f.optional" class="opt">(可选)</span></label>
                <div v-if="f.help" class="cred-help">{{ f.help }}</div>
              </div>
              <t-switch :model-value="asBool(cur.config[f.key])" @change="v => cur.config[f.key] = v" />
            </div>
          </template>
          <template v-else>
            <label>{{ f.label }} <span v-if="f.optional" class="opt">(可选)</span></label>
            <div v-if="f.help" class="cred-help">{{ f.help }}</div>
            <t-select
              v-if="f.type === 'select'"
              v-model="cur.config[f.key]"
              :options="optionsOf(f)"
              :placeholder="f.placeholder || '请选择'"
              clearable
            />
            <t-select
              v-else-if="f.type === 'multiselect'"
              v-model="cur.config[f.key]"
              :options="optionsOf(f)"
              :placeholder="f.placeholder || '请选择（可多选）'"
              multiple
              clearable
            />
            <t-input-number
              v-else-if="f.type === 'number'"
              v-model="cur.config[f.key]"
              theme="column"
              :placeholder="f.placeholder"
              style="width: 100%"
            />
            <t-input
              v-else
              v-model="cur.config[f.key]"
              :type="f.type === 'password' && !showKey[f.key] ? 'password' : 'text'"
              :placeholder="f.placeholder"
            >
              <template v-if="f.type === 'password'" #suffix-icon>
                <t-icon :name="showKey[f.key] ? 'browse' : 'browse-off'" style="cursor:pointer" @click="showKey[f.key] = !showKey[f.key]" />
              </template>
            </t-input>
          </template>
        </div>
      </div>

      <!-- 聊天节奏：合并进平台设置，避免用户在两个入口间跳 -->
      <div v-if="cur.type !== 'web'" class="rhythm-box">
        <h4 class="sec-title">聊天节奏</h4>
        <div class="rh-note">Web 渠道不参与节奏控制，此配置仅对「{{ cur.name }}」生效。单聊默认即时回复，群聊 / 频道默认受控防刷屏。</div>
        <div class="rh-top">
          <div>
            <div class="rh-top-title">启用「{{ cur.name }}」聊天节奏</div>
            <div class="rh-top-desc">关闭后该平台所有会话类型均不应用节奏控制</div>
          </div>
          <t-switch v-model="cur.rhythm.enabled" size="large" />
        </div>
        <template v-if="cur.rhythm.enabled">
          <div v-for="ct in rhythmChatTypes" :key="ct.key" class="rh-card">
            <div class="rh-row">
              <div>
                <div class="rh-card-title">{{ ct.label }}</div>
                <div class="rh-card-desc">{{ ct.key === 'private' ? '关闭 = 即时回复（推荐）；开启 = 受节奏控制' : '建议开启：避免刷屏' }}</div>
              </div>
              <t-switch v-model="cur.rhythm[ct.key].enabled" size="large" />
            </div>
            <template v-if="cur.rhythm[ct.key].enabled">
              <div class="rh-card-desc" style="margin-top:12px">发言倾向（0.01 = 安静，1.0 = 回复所有消息）</div>
              <div class="rh-slider">
                <t-slider v-model="cur.rhythm[ct.key].speakTendency" :min="0.01" :max="1" :step="0.01" style="flex:1" />
                <span class="rh-slider-val">{{ (cur.rhythm[ct.key].speakTendency || 0).toFixed(2) }}</span>
              </div>
              <div class="rh-grid2" style="margin-top:14px">
                <div class="rh-field">
                  <label>防抖静默等待（秒）</label>
                  <t-input-number v-model="cur.rhythm[ct.key].debounce.quietWait" :min="0" theme="normal" style="width:100%" />
                </div>
                <div class="rh-field">
                  <label>连续发言上限（0 = 不限）</label>
                  <t-input-number v-model="cur.rhythm[ct.key].interrupt.maxConsecutive" :min="0" theme="normal" style="width:100%" />
                </div>
              </div>
            </template>
          </div>
        </template>
      </div>

      <div class="pd-footer">
        <t-button theme="primary" data-testid="platform-save" @click="save">保存</t-button>
        <t-button theme="danger" variant="text" class="pd-del" @click="remove">删除</t-button>
      </div>
    </div>
    <t-empty v-else description="请选择或添加一个平台" class="plat-empty" />

    <!-- 添加平台弹窗（图1：图标平台列表） -->
    <t-dialog v-model:visible="addVisible" header="添加平台" :width="440" dialogClassName="add-dialog">
      <div class="add-list">
        <div
          v-for="t in types"
          :key="t.type"
          class="add-item"
          :class="{ active: addType === t.type }"
          @click="addType = t.type"
          @dblclick="confirmAdd"
        >
          <div class="ai-icon" :style="iconTint(t.color)">{{ iconText(t, t.name) }}</div>
          <div class="ai-body">
            <span class="ai-name">{{ t.name }}</span>
            <span v-if="t.description" class="ai-desc">{{ t.description }}</span>
          </div>
        </div>
      </div>
      <template #footer>
        <t-button variant="outline" @click="addVisible = false">取消</t-button>
        <t-button theme="primary" :disabled="!addType" @click="confirmAdd">添加</t-button>
      </template>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, reactive } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botPlatformApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const list = ref([])
const cur = ref(null)
const types = ref([])
const showKey = reactive({})

const fields = computed(() => types.value.find(t => t.type === cur.value?.type)?.fields || [])
const curMeta = computed(() => types.value.find(t => t.type === cur.value?.type))
function metaOf(type) { return types.value.find(t => t.type === type) }

// 聊天节奏：合并进平台设置（private/group/channel 三套参数），与后端 BotRhythmConfig 对齐。
const rhythmChatTypes = [
  { key: 'private', label: '单聊（1对1）' },
  { key: 'group', label: '群聊 / 超级群' },
  { key: 'channel', label: '频道' }
]
function defaultRhythm() {
  const mk = (enabled, speakTendency) => ({
    enabled,
    debounce: { quietWait: 3, maxWait: 15 },
    timing: { enabled: true },
    speakTendency,
    interrupt: { enabled: true, maxConsecutive: 3, maxRounds: 5 },
    idleComp: { enabled: false, idleWindow: 30, minIdle: 10 }
  })
  return { enabled: true, private: mk(false, 0.4), group: mk(true, 0.4), channel: mk(true, 0.4) }
}
// 补齐缺失的节奏子配置，避免空指针
function ensureRhythmDefaults(p) {
  if (!p) return
  if (!p.rhythm || typeof p.rhythm !== 'object') p.rhythm = defaultRhythm()
  const def = defaultRhythm()
  if (!p.rhythm.private) p.rhythm.private = def.private
  if (!p.rhythm.group) p.rhythm.group = def.group
  if (!p.rhythm.channel) p.rhythm.channel = def.channel
  for (const ct of ['private', 'group', 'channel']) {
    const s = p.rhythm[ct]
    if (!s.debounce) s.debounce = { quietWait: 3, maxWait: 15 }
    if (!s.interrupt) s.interrupt = { enabled: true, maxConsecutive: 3, maxRounds: 5 }
    if (typeof s.speakTendency !== 'number') s.speakTendency = 0.4
  }
}

// 图标文案：后端 icon 多为平台英文名（如 "misskey"），整串塞进小圆圈会溢出。
// 规则：icon 为单个字符/emoji 时直接用；否则（多字符或缺省）取首字母大写。
function iconText(meta, fallbackName) {
  const raw = meta?.icon || meta?.name || fallbackName || '?'
  const chars = Array.from(String(raw).trim())
  if (chars.length <= 1) return raw || '?'
  return chars[0].toUpperCase()
}
function iconTint(color) {
  if (color) return { background: color + '22', color }
  return { background: 'var(--bp-surface-fill)', color: 'var(--bp-label-tertiary)' }
}

// 兼容后端 "boolean" 与 mock "switch" 两种开关类型名
function isSwitch(f) { return f.type === 'switch' || f.type === 'boolean' }
// 将 config 中可能为 undefined / "true" / "false" 字符串的值归一为布尔
function asBool(v) { return v === true || v === 'true' }
// select 选项：兼容 ["a","b"] 或 [{label,value}]，并过滤空串占位项给出可读文案
function optionsOf(f) {
  const opts = f.options || []
  return opts.map(o => {
    if (o && typeof o === 'object') return o
    return { label: o === '' ? '（默认）' : o, value: o }
  })
}

// 选中平台时，用字段定义补齐缺省 config，避免开关/选择框初值为 undefined
function ensureConfigDefaults(p) {
  if (!p) return
  if (!p.config) p.config = {}
  const defs = types.value.find(t => t.type === p.type)?.fields || []
  for (const f of defs) {
    if (p.config[f.key] === undefined) {
      if (isSwitch(f)) p.config[f.key] = asBool(p.placeholder ?? f.placeholder)
      else if (f.type === 'multiselect') p.config[f.key] = []
      else if (f.type === 'number') p.config[f.key] = f.placeholder ? Number(f.placeholder) : undefined
    }
  }
}

async function load() {
  const [cat, l] = await Promise.all([botPlatformApi.toolCatalog(), botPlatformApi.list(props.botId)])
  types.value = cat.types
  list.value = l
  cur.value = l[0] || null
  ensureConfigDefaults(cur.value)
  ensureRhythmDefaults(cur.value)
}
onMounted(load)

function select(p) { cur.value = p; ensureConfigDefaults(p); ensureRhythmDefaults(p) }

function statusText(p) {
  if (p.enabled) return '已启用'
  if (p.configured) return '已停用'
  return '未配置'
}

async function save() {
  await botPlatformApi.update(props.botId, cur.value.id, {
    enabled: !!cur.value.enabled, config: cur.value.config, tools: cur.value.tools, name: cur.value.name, rhythm: cur.value.rhythm
  })
  cur.value.configured = true
  const i = list.value.findIndex(x => x.id === cur.value.id)
  if (i >= 0) list.value[i] = { ...cur.value }
  MessagePlugin.success(cur.value.enabled ? '已保存并启用' : '已保存（平台已停用）')
}

function remove() {
  const dlg = DialogPlugin.confirm({
    header: '删除平台', body: `确认删除「${cur.value.name}」？`, theme: 'warning',
    onConfirm: async () => {
      await botPlatformApi.remove(props.botId, cur.value.id)
      dlg.destroy()
      MessagePlugin.success('已删除')
      await load()
    }
  })
}

const addVisible = ref(false)
const addType = ref('')
function openAdd() { addType.value = types.value[0]?.type || ''; addVisible.value = true }
async function confirmAdd() {
  const meta = types.value.find(t => t.type === addType.value)
  const created = await botPlatformApi.create(props.botId, { type: addType.value, name: meta?.name, config: {}, tools: [] })
  addVisible.value = false
  await load()
  cur.value = list.value.find(p => p.id === created.id) || cur.value
  ensureConfigDefaults(cur.value)
  ensureRhythmDefaults(cur.value)
  MessagePlugin.success('平台已添加')
}
</script>

<style scoped>
.plat-wrap { display: flex; gap: 20px; height: 100%; }
/* 左列表 */
.plat-list {
  width: 240px; flex-shrink: 0; display: flex; flex-direction: column; gap: 8px;
  border-right: var(--bp-hairline); padding-right: 16px;
}
.plat-item {
  display: flex; align-items: center; gap: 10px; padding: 12px 14px;
  border: var(--bp-hairline); border-radius: 10px; cursor: pointer; background: var(--bp-surface);
  transition: background var(--bp-duration) var(--bp-ease-out), transform var(--bp-duration) var(--bp-ease-out), box-shadow var(--bp-duration) var(--bp-ease-out);
}
.plat-item:active { transform: scale(var(--bp-press-scale)); }
.plat-item.active { border-color: var(--bp-separator); box-shadow: var(--bp-shadow-md); background: var(--bp-bg-subtle); }
.pi-icon {
  width: 34px; height: 34px; border-radius: 50%; flex-shrink: 0;
  display: flex; align-items: center; justify-content: center;
  font-size: 16px; font-weight: 700; line-height: 1; overflow: hidden; text-transform: uppercase;
}
.pi-main { flex: 1; min-width: 0; }
.pi-name { font-size: 14px; font-weight: 600; color: var(--bp-label); }
.pi-sub { font-size: 12px; color: var(--bp-label-tertiary); margin-top: 2px; }
.plat-add {
  display: flex; align-items: center; justify-content: center; gap: 6px;
  padding: 11px; border: 1px dashed var(--bp-separator); border-radius: 10px; background: var(--bp-surface);
  color: var(--bp-label-secondary); font-size: 14px; cursor: pointer;
  transition: border-color var(--bp-duration) var(--bp-ease-out), transform var(--bp-duration) var(--bp-ease-out), background var(--bp-duration) var(--bp-ease-out);
}
.plat-add:hover { border-color: var(--bp-label-tertiary); }
.plat-add:active { transform: scale(var(--bp-press-scale)); }
/* 右详情 */
.plat-detail { flex: 1; min-width: 0; overflow-y: auto; padding-right: 4px; }
.pd-head { display: flex; align-items: center; gap: 12px; margin-bottom: 24px; }
.pd-avatar {
  width: 40px; height: 40px; border-radius: 50%; flex-shrink: 0;
  display: flex; align-items: center; justify-content: center;
  font-size: 18px; font-weight: 700; line-height: 1; overflow: hidden; text-transform: uppercase;
}
.pd-title { flex: 1; }
.pd-name { font-size: 16px; font-weight: 600; }
.pd-id { font-size: 12px; color: var(--bp-label-tertiary); margin-top: 2px; }
.pd-enable { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; margin-bottom: 24px; padding: 14px 16px; border-radius: 12px; background: var(--bp-bg-subtle); border: var(--bp-hairline); }
.pd-enable-title { font-size: 15px; font-weight: 600; color: var(--bp-label); }
.pd-enable-desc { font-size: 12px; color: var(--bp-label-tertiary); margin-top: 4px; line-height: 1.45; }
.pi-sub.on { color: var(--bp-accent, #0071e3); }
.pi-sub.off { color: var(--bp-label-secondary); }
.sec-title { font-size: 14px; font-weight: 600; margin: 0 0 14px; color: var(--bp-label); }
.cred-form { display: flex; flex-direction: column; gap: 18px; margin-bottom: 28px; }
.cred-item label { display: block; font-size: 13px; font-weight: 600; margin-bottom: 6px; }
.cred-item label .opt { font-weight: 400; color: var(--bp-label-tertiary); font-size: 12px; margin-left: 4px; }
.cred-help { font-size: 12px; color: var(--bp-label-tertiary); margin-bottom: 8px; }
/* 开关型字段：标签左、开关右，垂直居中 */
.cred-item-inline { padding: 4px 0; }
.cred-switch-row { display: flex; align-items: center; justify-content: space-between; gap: 16px; }
.cred-switch-label { flex: 1; min-width: 0; }
.cred-switch-label label { margin-bottom: 2px; }
.cred-switch-label .cred-help { margin-bottom: 0; }
.pd-footer { margin-top: 20px; padding-top: 18px; border-top: var(--bp-hairline); display: flex; justify-content: flex-end; gap: 12px; align-items: center; }
.pd-del { margin-right: auto; }
.plat-empty { margin: 60px auto; }
/* 聊天节奏（内嵌于平台详情） */
.rhythm-box { margin-top: 8px; }
.rh-note { font-size: 13px; color: var(--bp-label-secondary); background: var(--bp-bg-subtle); border: var(--bp-hairline); border-radius: 10px; padding: 12px 14px; margin-bottom: 18px; line-height: 1.6; }
.rh-top { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: 22px; }
.rh-top-title { font-size: 15px; font-weight: 600; }
.rh-top-desc { font-size: 13px; color: var(--bp-label-tertiary); margin-top: 4px; }
.rh-card { border: none; box-shadow: var(--bp-shadow-sm); border-radius: 12px; padding: 18px 20px; margin-bottom: 16px; background: var(--bp-surface); }
.rh-card-title { font-size: 14px; font-weight: 600; }
.rh-card-desc { font-size: 12px; color: var(--bp-label-tertiary); margin-top: 4px; }
.rh-row { display: flex; align-items: flex-start; justify-content: space-between; }
.rh-grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-top: 14px; }
.rh-field label { display: block; font-size: 13px; color: var(--bp-label-secondary); margin-bottom: 6px; }
.rh-slider { display: flex; align-items: center; gap: 16px; margin-top: 16px; }
.rh-slider-val { font-size: 14px; color: var(--bp-label); width: 42px; text-align: right; }
/* 添加平台弹窗 */
.add-list {
  display: flex; flex-direction: column; gap: 2px;
  max-height: 420px; overflow-y: auto;
}
.add-list::-webkit-scrollbar { width: 6px; }
.add-list::-webkit-scrollbar-thumb { background: var(--bp-separator); border-radius: 3px; }
.add-item {
  display: flex; align-items: center; gap: 12px; padding: 8px 12px;
  border-radius: 10px; cursor: pointer; transition: background var(--bp-duration) var(--bp-ease-out), transform var(--bp-duration) var(--bp-ease-out);
}
.add-item:hover { background: var(--bp-bg-subtle); }
.add-item:active { transform: scale(var(--bp-press-scale)); }
.add-item.active { background: var(--bp-surface-fill); }
.ai-icon {
  width: 36px; height: 36px; border-radius: 50%; flex-shrink: 0;
  display: flex; align-items: center; justify-content: center;
  font-size: 17px; font-weight: 700; line-height: 1; overflow: hidden; text-transform: uppercase;
}
.ai-name { font-size: 15px; color: var(--bp-label); }
.ai-body { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
.ai-desc { font-size: 12px; color: var(--bp-label-tertiary); line-height: 1.3; }
</style>

<!-- 弹窗渲染在 body 下，scoped 命中不到，用全局样式 -->
<style>
.add-dialog.t-dialog { padding: 10px; }
.add-dialog .t-dialog__header { padding: 0; margin-bottom: 6px; }
.add-dialog .t-dialog__body { padding: 0; }
.add-dialog .t-dialog__footer { padding: 8px 0 0; }
</style>
