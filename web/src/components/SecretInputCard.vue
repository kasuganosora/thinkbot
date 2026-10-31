<template>
  <section class="secret-card" aria-label="密钥输入" data-testid="secret-input-card">
    <div class="secret-heading">密钥输入<span v-if="request.name"> · {{ request.name }}</span></div>
    <p v-if="request.purpose" class="secret-purpose">用途：{{ request.purpose }}</p>
    <form v-if="state === 'ready' || state === 'submitting'" autocomplete="off" @submit.prevent="submit(false)">
      <label class="secret-label">密钥
        <input v-model="value" type="password" autocomplete="off" autocapitalize="none" :spellcheck="false"
          :disabled="state === 'submitting'" placeholder="输入密钥（12 到 16384 字节）" data-testid="secret-input-value" />
      </label>
      <p class="secret-note">请仅在此输入，不要粘贴到聊天框。请求过期或服务重启后，请重新发起密钥请求。</p>
      <div class="secret-actions">
        <button type="submit" :disabled="state === 'submitting' || !validValue" data-testid="secret-input-submit">{{ state === 'submitting' ? '处理中…' : '保存并继续' }}</button>
        <button type="button" class="secondary" :disabled="state === 'submitting'" data-testid="secret-input-cancel" @click="submit(true)">取消请求</button>
      </div>
    </form>
    <p v-if="notice" class="secret-notice" :class="{ warning: state === 'failed' || continuationFailed }" role="status" data-testid="secret-input-status">{{ notice }}</p>
  </section>
</template>

<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { request as httpRequest } from '@/api/http'
import { useBotStore } from '@/stores/bot'
import { submitSecretForm } from '@/utils/secretInput'

const props = defineProps({
  request: { type: Object, required: true },
  botId: { type: [String, Number], required: true },
  sessionId: { type: [String, Number], required: true },
})
const store = useBotStore()
// Keep the raw value in this component only. Never bind it to the chat draft.
const value = ref('')
const state = ref('ready')
const continuationFailed = ref(false)
let alive = true
let generation = 0
let timer
const validValue = computed(() => {
  const encoder = new TextEncoder()
  return encoder.encode(value.value.trim()).length >= 12 && encoder.encode(value.value).length <= 16384
})
const notice = computed(() => {
  if (state.value === 'stored') return continuationFailed.value ? '密钥已保存，但继续任务未确认。请在聊天中说明“密钥已提供，请继续”，不要再次发送原值。' : '密钥已保存，已发送安全提示继续任务。'
  if (state.value === 'cancelled') return '请求已取消。'
  if (state.value === 'expired') return '请求已过期。请重新发起密钥请求，不要将原值发送到聊天中。'
  if (state.value === 'failed') return '提交未确认，请重新发起密钥请求。输入已清空，不会自动转为聊天消息。'
  return ''
})
function checkExpiry() {
  if (state.value === 'ready' && Date.now() >= props.request.expires_at * 1000) {
    value.value = ''
    state.value = 'expired'
  }
}
watch(() => [props.request.question_id, props.botId, props.sessionId], () => {
  generation++
  value.value = ''
  state.value = 'ready'
  continuationFailed.value = false
  checkExpiry()
}, { immediate: true })

async function submit(cancel) {
  checkExpiry()
  if (state.value !== 'ready' || (!cancel && !validValue.value)) return
  const currentGeneration = generation
  const botId = String(props.botId)
  const sessionId = String(props.sessionId)
  let submittedValue = value.value
  value.value = ''
  state.value = 'submitting'
  const isCurrent = () => alive && currentGeneration === generation
    && String(store.activeBotId) === botId && String(store.activeSessionId) === sessionId
  const result = await submitSecretForm({ questionId: props.request.question_id, botId, sessionId, value: submittedValue, cancel }, {
    post: httpRequest, isCurrent,
    continueChat: text => store.replying ? store.appendToCurrentReply(text) : store.sendMessage(text),
  })
  submittedValue = ''
  if (!alive || currentGeneration !== generation) return
  state.value = result.status
  continuationFailed.value = result.status === 'stored' && !result.continued
  if (result.completion) {
    try {
      const delivery = await result.completion
      if (alive && currentGeneration === generation && !['completed', 'accepted'].includes(delivery?.status)) continuationFailed.value = true
    } catch {
      if (alive && currentGeneration === generation) continuationFailed.value = true
    }
  }
}
onMounted(() => { timer = setInterval(checkExpiry, 1000) })
onBeforeUnmount(() => { alive = false; generation++; value.value = ''; clearInterval(timer) })
</script>

<style scoped>
.secret-card { margin-top: 12px; padding: 16px; border: var(--bp-hairline); border-radius: var(--bp-radius-sm); background: var(--bp-surface); max-width: 520px; }
.secret-heading { font-weight: 600; color: var(--bp-label); }
.secret-heading span, .secret-purpose { overflow-wrap: anywhere; }
.secret-purpose { margin: 8px 0 12px; font-size: 13px; color: var(--bp-label-secondary); }
.secret-label { display: grid; gap: 6px; color: var(--bp-label); font-size: 13px; }
.secret-label input { width: 100%; min-width: 0; box-sizing: border-box; border: var(--bp-hairline); border-radius: 6px; padding: 10px; color: var(--bp-label); background: var(--bp-bg, transparent); font: inherit; }
.secret-note, .secret-notice { font-size: 12px; line-height: 1.6; color: var(--bp-label-secondary); margin: 10px 0; }
.secret-notice:last-child { margin-bottom: 0; }
.secret-notice.warning { color: var(--bp-danger); }
.secret-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.secret-actions button { border: 0; border-radius: 6px; padding: 8px 12px; background: var(--bp-accent); color: white; cursor: pointer; font: inherit; font-size: 12px; }
.secret-actions button.secondary { background: var(--bp-accent-soft); color: var(--bp-accent); }
.secret-actions button:disabled { opacity: .5; cursor: default; }
input:focus-visible, button:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
</style>
