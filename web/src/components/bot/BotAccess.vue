<template>
  <div class="acc-wrap" data-testid="bot-access">
    <div class="acc-head">
      <h3 class="acc-title">访问控制</h3>
      <p class="acc-desc">按优先级排序的规则控制谁可以触发此 Bot 的聊天。首条匹配规则生效；无规则匹配时采用默认行为。</p>
    </div>

    <!-- 默认行为 -->
    <div class="acc-card">
      <div class="card-title">默认行为</div>
      <div class="card-sub">当没有规则匹配入站消息时应用此策略。</div>
      <div class="def-row">
        <t-radio-group v-model="def" variant="default-filled">
          <t-radio-button value="deny"><span class="dot deny" />拒绝</t-radio-button>
          <t-radio-button value="allow"><span class="dot allow" />允许</t-radio-button>
        </t-radio-group>
        <t-button theme="default" @click="saveDefault">保存</t-button>
      </div>
    </div>

    <!-- 规则 -->
    <div class="acc-card">
      <div class="rule-head">
        <div>
          <div class="card-title">规则</div>
          <div class="card-sub">拖动左侧手柄调整顺序。数字越小越先评估，首条匹配规则的行为生效。</div>
        </div>
        <t-button theme="primary" @click="openAdd"><template #icon><t-icon name="add" /></template>添加规则</t-button>
      </div>

      <div v-if="rules.length" class="rule-list">
        <div v-for="(r, i) in rules" :key="r.id" class="rule-item">
          <t-icon name="drag-move" class="rule-drag" />
          <span class="rule-no">{{ i + 1 }}</span>
          <t-tag :theme="r.action === 'allow' ? 'success' : 'danger'" variant="light">{{ r.action === 'allow' ? '允许' : '拒绝' }}</t-tag>
          <span class="rule-cond">{{ condText(r) }}</span>
          <div class="rule-ops">
            <t-button variant="text" size="small" @click="move(i, -1)" :disabled="i === 0">上移</t-button>
            <t-button variant="text" size="small" @click="move(i, 1)" :disabled="i === rules.length - 1">下移</t-button>
            <t-button variant="text" theme="danger" size="small" @click="removeRule(i)">删除</t-button>
          </div>
        </div>
      </div>
      <t-empty v-else description="暂无规则，所有消息按默认行为处理" />
    </div>

    <!-- 添加规则弹窗 -->
    <t-dialog v-model:visible="addVisible" header="添加规则" :width="480" @confirm="confirmAdd">
      <t-form :data="form" label-align="top">
        <t-form-item label="匹配字段">
          <t-select v-model="form.field" :options="fieldOptions" />
        </t-form-item>
        <t-form-item label="匹配值">
          <t-input v-model="form.value" placeholder="如 telegram / user:123 / 关键词" />
        </t-form-item>
        <t-form-item label="行为">
          <t-radio-group v-model="form.action">
            <t-radio-button value="allow">允许</t-radio-button>
            <t-radio-button value="deny">拒绝</t-radio-button>
          </t-radio-group>
        </t-form-item>
      </t-form>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { botAccessApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const def = ref('allow')
const rules = ref([])

const fieldOptions = [
  { label: '平台', value: 'platform' },
  { label: '用户 ID', value: 'userId' },
  { label: '关键词', value: 'keyword' }
]
const fieldLabel = { platform: '平台', userId: '用户 ID', keyword: '关键词' }
function condText(r) { return `${fieldLabel[r.field] || r.field} = ${r.value}` }

async function load() {
  const a = await botAccessApi.get(props.botId)
  def.value = a.default
  rules.value = a.rules
}
onMounted(load)

async function saveDefault() {
  await botAccessApi.update(props.botId, { default: def.value })
  MessagePlugin.success('默认行为已保存')
}

async function persistRules() {
  await botAccessApi.update(props.botId, { rules: rules.value })
}
function move(i, d) {
  const j = i + d
  ;[rules.value[i], rules.value[j]] = [rules.value[j], rules.value[i]]
  persistRules()
}
function removeRule(i) {
  rules.value.splice(i, 1)
  persistRules()
  MessagePlugin.success('规则已删除')
}

const addVisible = ref(false)
const form = ref({ field: 'platform', value: '', action: 'allow' })
function openAdd() { form.value = { field: 'platform', value: '', action: 'allow' }; addVisible.value = true }
async function confirmAdd() {
  if (!form.value.value.trim()) return MessagePlugin.warning('请填写匹配值')
  rules.value.push({ id: `rule_${Date.now()}`, ...form.value })
  await persistRules()
  addVisible.value = false
  MessagePlugin.success('规则已添加')
}
</script>

<style scoped>
.acc-wrap { max-width: 860px; }
.acc-head { margin-bottom: 20px; }
.acc-title { font-size: 16px; font-weight: 600; margin: 0 0 6px; }
.acc-desc { font-size: 13px; color: #888; margin: 0; }
.acc-card { border: 1px solid #ececec; border-radius: 12px; padding: 18px 20px; margin-bottom: 20px; background: #fff; }
.card-title { font-size: 14px; font-weight: 600; }
.card-sub { font-size: 12px; color: #999; margin-top: 4px; }
.def-row { display: flex; align-items: center; gap: 14px; margin-top: 14px; }
.dot { display: inline-block; width: 7px; height: 7px; border-radius: 50%; margin-right: 6px; vertical-align: middle; }
.dot.deny { background: #bbb; }
.dot.allow { background: #34c759; }
.rule-head { display: flex; align-items: flex-start; justify-content: space-between; }
.rule-list { margin-top: 16px; display: flex; flex-direction: column; gap: 8px; }
.rule-item { display: flex; align-items: center; gap: 12px; padding: 10px 12px; border: 1px solid #f0f0f0; border-radius: 8px; }
.rule-drag { color: #ccc; cursor: grab; }
.rule-no { width: 20px; text-align: center; color: #999; font-size: 13px; }
.rule-cond { flex: 1; font-size: 13px; color: #333; }
.rule-ops { display: flex; gap: 2px; }
</style>
