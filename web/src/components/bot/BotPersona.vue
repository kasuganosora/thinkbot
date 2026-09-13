<template>
  <div class="persona-wrap" data-testid="bot-persona">
    <div class="persona-head">
      <div>
        <h3 class="ph-title">人格（SOUL.md）</h3>
        <p class="ph-desc">定义 Bot 的性格、身份与行为准则。保存后写入工作空间，运行中的 Bot 下一轮对话即生效，无需重启。</p>
      </div>
      <t-space>
        <t-button variant="outline" :loading="loading" @click="load">刷新</t-button>
        <t-button variant="outline" @click="resetDefault">恢复默认模板</t-button>
        <t-button theme="primary" :loading="saving" data-testid="persona-save-btn" @click="save">保存</t-button>
      </t-space>
    </div>

    <t-alert v-if="exists === false && !dirty" theme="info" class="hint-bar">
      尚未创建人格文件，下面是默认模板。保存后才会写入 SOUL.md。
    </t-alert>
    <t-alert v-else-if="hotReloaded" theme="success" class="hint-bar">
      Bot 正在运行，保存后会立即热加载。
    </t-alert>
    <t-alert v-else theme="warning" class="hint-bar">
      Bot 当前未运行。保存会写入文件，启动后才会生效。
    </t-alert>

    <t-alert v-if="warning" theme="warning" class="hint-bar" data-testid="persona-scan-warning">
      安全扫描告警：{{ warning }}
    </t-alert>

    <t-textarea
      v-model="content"
      :autosize="{ minRows: 18, maxRows: 32 }"
      class="persona-editor"
      data-testid="persona-editor"
      placeholder="用 Markdown 描述人格…"
    />
    <div class="persona-meta">
      <span>{{ content.length }} 字</span>
      <span v-if="oversize" class="oversize">超过 {{ maxBytes }} 字节上限，无法保存</span>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, watch } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { soulApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const maxBytes = 20000
const loading = ref(false)
const saving = ref(false)
const content = ref('')
const savedContent = ref('')
const exists = ref(null)
const hotReloaded = ref(false)
const warning = ref('')

const dirty = computed(() => content.value !== savedContent.value)
const oversize = computed(() => new TextEncoder().encode(content.value).length > maxBytes)

async function load() {
  loading.value = true
  try {
    const resp = await soulApi.get(props.botId)
    content.value = resp.content || ''
    savedContent.value = content.value
    exists.value = !!resp.exists
    hotReloaded.value = !!resp.hotReloaded
    warning.value = resp.warning || ''
  } catch (e) {
    MessagePlugin.error('加载人格失败：' + (e.message || '请稍后重试'))
  } finally {
    loading.value = false
  }
}

async function save() {
  if (oversize.value) {
    MessagePlugin.warning(`内容超过 ${maxBytes} 字节上限`)
    return
  }
  saving.value = true
  try {
    const resp = await soulApi.update(props.botId, { content: content.value })
    savedContent.value = content.value
    exists.value = true
    hotReloaded.value = !!resp.hotReloaded
    warning.value = resp.warning || ''
    if (resp.warning) {
      MessagePlugin.warning('已保存，但安全扫描发现可疑内容')
    } else if (resp.hotReloaded) {
      MessagePlugin.success('人格已保存并热加载')
    } else {
      MessagePlugin.success('人格已保存，启动 Bot 后生效')
    }
  } catch (e) {
    MessagePlugin.error('保存失败：' + (e.message || '请稍后重试'))
  } finally {
    saving.value = false
  }
}

function resetDefault() {
  const dlg = DialogPlugin.confirm({
    header: '恢复默认模板',
    body: '将用默认人格模板覆盖当前编辑内容（未保存前不会写入文件）。确认继续？',
    theme: 'warning',
    onConfirm: () => {
      content.value = defaultSoulTemplate
      warning.value = ''
      dlg.destroy()
    }
  })
}

const defaultSoulTemplate = `# Soul

You are a helpful AI assistant, a conversational agent that helps users get things done.

## Personality

- Friendly and approachable
- Concise and direct in communication
- Helpful and knowledgeable

## Guidelines

- You must respond to the user in Chinese (中文), unless the user writes in another language — then match their language
- Be honest and transparent
- If you don't know something, say so. NEVER invent facts

<!-- Edit this file to customize your bot's personality. Changes are hot-reloaded. -->
`

watch(() => props.botId, load, { immediate: true })
</script>

<style scoped>
.persona-wrap { width: 100%; }
.persona-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 16px;
}
.ph-title { margin: 0 0 6px; font-size: 16px; font-weight: 600; }
.ph-desc { margin: 0; font-size: 13px; color: var(--bp-label-tertiary); max-width: 640px; line-height: 1.6; }
.hint-bar { margin-bottom: 12px; }
.persona-editor :deep(textarea) {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 13px;
  line-height: 1.55;
}
.persona-meta {
  display: flex;
  justify-content: space-between;
  margin-top: 8px;
  font-size: 12px;
  color: var(--bp-label-tertiary);
}
.oversize { color: var(--bp-danger); }
</style>
