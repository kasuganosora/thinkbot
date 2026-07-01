<template>
  <div class="mem-wrap" data-testid="bot-memory">
    <!-- 左列表 -->
    <div class="mem-list">
      <div class="ml-head">
        <span class="ml-title">记忆文件</span>
        <div class="ml-ops">
          <t-icon name="chart-bubble" @click="load" />
          <t-icon name="refresh" @click="load" />
        </div>
      </div>
      <t-input v-model="kw" placeholder="搜索记忆..." clearable class="ml-search">
        <template #prefix-icon><t-icon name="search" /></template>
      </t-input>
      <div class="ml-scroll">
        <div
          v-for="m in filtered"
          :key="m.id"
          class="ml-item"
          :class="{ active: cur && cur.id === m.id }"
          @click="select(m)"
        >
          <t-icon name="file" class="mi-icon" />
          <div class="mi-main">
            <div class="mi-title">{{ m.title }}</div>
            <div class="mi-preview">{{ m.content }}</div>
          </div>
        </div>
      </div>
      <button class="mem-new" @click="createNew"><t-icon name="add" /> 新建记忆</button>
    </div>

    <!-- 右编辑 -->
    <div v-if="cur" class="mem-editor">
      <div class="me-head">
        <t-icon name="file" />
        <div class="me-title">
          <div class="me-name">{{ cur.title }}</div>
          <div class="me-id">ID: {{ cur.id }} <t-icon name="copy" style="cursor:pointer" @click="copyId" /></div>
        </div>
        <t-icon name="delete" class="me-del" @click="remove" />
        <t-button theme="primary" :disabled="!dirty" @click="save">保存</t-button>
      </div>
      <t-textarea v-model="cur.content" :autosize="{ minRows: 18 }" class="me-area" @change="dirty = true" @input="dirty = true" />
    </div>
    <t-empty v-else description="选择左侧记忆查看或编辑" class="mem-empty" />
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botMemoryApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const list = ref([])
const cur = ref(null)
const kw = ref('')
const dirty = ref(false)

const filtered = computed(() => {
  const q = kw.value.trim().toLowerCase()
  if (!q) return list.value
  return list.value.filter(m => (m.title + m.content).toLowerCase().includes(q))
})

async function load() {
  list.value = await botMemoryApi.list(props.botId)
  if (!cur.value || !list.value.find(m => m.id === cur.value.id)) cur.value = list.value[0] || null
  dirty.value = false
}
onMounted(load)

function select(m) { cur.value = { ...m }; dirty.value = false }

async function save() {
  await botMemoryApi.update(props.botId, cur.value.id, { content: cur.value.content })
  const item = list.value.find(m => m.id === cur.value.id)
  if (item) item.content = cur.value.content
  dirty.value = false
  MessagePlugin.success('记忆已保存')
}

async function createNew() {
  const m = await botMemoryApi.create(props.botId, { content: '' })
  await load()
  cur.value = { ...m }
}

function remove() {
  const dlg = DialogPlugin.confirm({
    header: '删除记忆', body: `确认删除「${cur.value.title}」？`, theme: 'warning',
    onConfirm: async () => {
      await botMemoryApi.remove(props.botId, cur.value.id)
      dlg.destroy(); MessagePlugin.success('已删除'); cur.value = null; await load()
    }
  })
}

function copyId() {
  navigator.clipboard?.writeText(cur.value.id)
  MessagePlugin.success('已复制 ID')
}
</script>

<style scoped>
.mem-wrap { display: flex; gap: 16px; height: 100%; }
/* 左列表 */
.mem-list {
  width: 300px; flex-shrink: 0; display: flex; flex-direction: column;
  border: 1px solid #ececec; border-radius: 12px; overflow: hidden; background: #fff;
}
.ml-head { display: flex; align-items: center; justify-content: space-between; padding: 14px 16px 10px; }
.ml-title { font-size: 14px; font-weight: 600; }
.ml-ops { display: flex; gap: 12px; color: #888; }
.ml-ops :deep(.t-icon) { cursor: pointer; }
.ml-search { padding: 0 12px 10px; }
.ml-scroll { flex: 1; overflow-y: auto; padding: 0 8px; }
.ml-item { display: flex; gap: 10px; padding: 10px 10px; border-radius: 8px; cursor: pointer; }
.ml-item:hover { background: #f6f6f6; }
.ml-item.active { background: #f0f1f3; }
.mi-icon { color: #999; margin-top: 2px; }
.mi-main { flex: 1; min-width: 0; }
.mi-title { font-size: 13px; font-weight: 600; color: #1d1d1f; }
.mi-preview { font-size: 12px; color: #999; margin-top: 3px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.mem-new {
  display: flex; align-items: center; justify-content: center; gap: 6px;
  margin: 8px; padding: 10px; border: 1px solid #ececec; border-radius: 8px;
  background: #fff; cursor: pointer; font-size: 14px; color: #333;
}
.mem-new:hover { background: #f6f6f6; }
/* 右编辑 */
.mem-editor { flex: 1; min-width: 0; border: 1px solid #ececec; border-radius: 12px; display: flex; flex-direction: column; overflow: hidden; }
.me-head { display: flex; align-items: center; gap: 10px; padding: 14px 16px; border-bottom: 1px solid #f0f0f0; }
.me-title { flex: 1; min-width: 0; }
.me-name { font-size: 14px; font-weight: 600; }
.me-id { font-size: 12px; color: #999; margin-top: 2px; display: flex; align-items: center; gap: 6px; }
.me-del { color: #e34d59; cursor: pointer; }
.me-area { flex: 1; padding: 12px 16px; }
.me-area :deep(.t-textarea__inner) { border: none; box-shadow: none; font-size: 13px; line-height: 1.7; }
.mem-empty { margin: 60px auto; }
</style>
