<template>
  <t-card title="推理强度（按任务）" :bordered="false" class="card" data-testid="bot-reasoning">
    <div class="hint">
      主回复的推理强度见上方「模型参数」。这里为后台任务单独设置；不支持 reasoning_effort 的模型不会发送该参数，
      GLM-5.3 等只接受部分取值的模型会自动映射（如 medium→high）。保存后下一次调用即生效。
    </div>
    <t-form :data="tasks" label-align="top" class="form">
      <div class="row2">
        <t-form-item v-for="t in REASONING_TASKS" :key="t.key" :label="t.label">
          <div class="field">
            <t-select v-model="tasks[t.key]" :options="taskEffortOptions(t.key, defaults)" :data-testid="`reasoning-${t.key}`" />
            <div class="field-tip">{{ t.tip }}</div>
          </div>
        </t-form-item>
      </div>
    </t-form>
    <div class="footer">
      <t-button theme="primary" :loading="saving" data-testid="reasoning-save" @click="save">保存推理设置</t-button>
    </div>
  </t-card>
</template>

<script setup>
import { reactive, ref, onMounted, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { botReasoningApi } from '@/api/services'
import { REASONING_TASKS, taskEffortOptions, normalizeTasks } from '@/utils/reasoning'

const props = defineProps({ botId: { type: String, required: true } })

const tasks = reactive(normalizeTasks())
const defaults = ref({})
const saving = ref(false)

async function load() {
  try {
    const r = await botReasoningApi.get(props.botId)
    Object.assign(tasks, normalizeTasks(r?.tasks))
    defaults.value = r?.defaults || {}
  } catch (e) {
    MessagePlugin.error('加载推理设置失败：' + (e?.message || e))
  }
}

async function save() {
  saving.value = true
  try {
    const r = await botReasoningApi.update(props.botId, { tasks: { ...tasks } })
    Object.assign(tasks, normalizeTasks(r?.tasks))
    MessagePlugin.success('推理设置已保存')
  } catch (e) {
    MessagePlugin.error('保存失败：' + (e?.message || e))
  } finally {
    saving.value = false
  }
}

onMounted(load)
watch(() => props.botId, load)
</script>

<style scoped>
.card { margin: 20px 0; }
.hint { font-size: 12px; line-height: 1.6; color: var(--bp-label-tertiary); margin-bottom: 12px; }
.row2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.field { width: 100%; }
.field-tip { margin-top: 6px; font-size: 12px; line-height: 1.5; color: var(--bp-label-tertiary); }
.footer { display: flex; justify-content: flex-end; }
</style>
