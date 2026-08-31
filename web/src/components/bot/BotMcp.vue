<template>
  <div class="mcp-wrap" data-testid="bot-mcp">
    <!-- 左栏：搜索 + 列表 + 底部按钮 -->
    <aside class="mcp-side">
      <div class="side-search">
        <t-input v-model="keyword" placeholder="搜索 MCP 服务器...">
          <template #suffix-icon><t-icon name="search" /></template>
        </t-input>
      </div>

      <div class="side-list">
        <div
          v-for="s in filtered"
          :key="s.id"
          class="srv-item"
          :class="{ active: cur && cur.id === s.id }"
          @click="select(s)"
        >
          <span class="dot" :class="s.status"></span>
          <span class="srv-name">{{ s.name }}</span>
          <t-tag v-if="s.status === 'draft'" size="small" variant="light">草稿</t-tag>
          <t-tag v-else-if="s.status === 'running'" size="small" theme="success" variant="light">运行中</t-tag>
          <t-tag v-else-if="s.status === 'disabled'" size="small" variant="light">已停用</t-tag>
          <t-tag v-else-if="s.status === 'error'" size="small" theme="danger" variant="light">异常</t-tag>
        </div>
        <div v-if="filtered.length === 0 && keyword" class="side-empty">无匹配结果</div>
      </div>

      <div class="side-foot">
        <t-button theme="primary" block @click="openAdd">
          <template #icon><t-icon name="add" /></template>
          添加
        </t-button>
        <t-button variant="outline" @click="openImport">导入</t-button>
      </div>
    </aside>

    <!-- 右栏 -->
    <section class="mcp-main">
      <!-- 空状态 -->
      <div v-if="servers.length === 0" class="empty">
        <div class="empty-icon"><t-icon name="usb" size="28px" /></div>
        <div class="empty-title">暂无 MCP 服务器</div>
        <div class="empty-desc">添加 MCP 服务器以扩展 Bot 的能力</div>
        <t-button variant="outline" @click="openAdd">添加</t-button>
      </div>

      <!-- 未选中（有服务器但没选） -->
      <div v-else-if="!cur" class="empty">
        <div class="empty-icon"><t-icon name="usb" size="28px" /></div>
        <div class="empty-desc">从左侧选择一个 MCP 服务器进行配置</div>
      </div>

      <!-- 详情编辑 -->
      <div v-else class="detail">
        <div class="detail-head">
          <span class="dh-name">{{ cur.name }}</span>
          <t-button theme="danger" size="small" @click="remove">删除</t-button>
        </div>

        <div class="grid2">
          <div class="field">
            <label class="lbl">名称</label>
            <t-input v-model="cur.name" placeholder="输入名称" />
          </div>
          <div class="field">
            <label class="lbl">类型</label>
            <t-select v-model="cur.type" :options="typeOptions" />
          </div>
        </div>

        <!-- Stdio 字段 -->
        <template v-if="cur.type === 'stdio'">
          <div class="field">
            <label class="lbl">命令</label>
            <t-input v-model="cur.command" placeholder="输入启动命令" />
          </div>
          <div class="field">
            <label class="lbl">参数</label>
            <t-tag-input v-model="cur.args" placeholder="输入后按 Enter 添加" :excess-tags-display-type="'break-line'" />
          </div>
          <div class="field">
            <label class="lbl">环境变量</label>
            <div class="env-list">
              <div v-for="(row, i) in envRows" :key="i" class="env-row">
                <t-input v-model="row.key" placeholder="KEY" style="flex:1" />
                <span class="env-eq">=</span>
                <t-input v-model="row.value" placeholder="VALUE" style="flex:1" />
                <t-button variant="text" theme="danger" shape="square" @click="removeEnv(i)">
                  <t-icon name="delete" />
                </t-button>
              </div>
              <t-button variant="outline" size="small" @click="addEnv">
                <template #icon><t-icon name="add" /></template>
                添加
              </t-button>
            </div>
          </div>
          <div class="field">
            <label class="lbl">工作目录</label>
            <t-input v-model="cur.cwd" placeholder="输入工作目录路径" />
          </div>
        </template>

        <!-- SSE / HTTP 字段 -->
        <template v-else>
          <div class="field">
            <label class="lbl">服务地址 (URL)</label>
            <t-input v-model="cur.url" :placeholder="cur.type === 'sse' ? 'https://example.com/sse' : 'https://example.com/mcp'" />
          </div>
          <div class="field">
            <label class="lbl">请求头 (Header)</label>
            <div class="env-list">
              <div v-for="(row, i) in headerRows" :key="i" class="env-row">
                <t-input v-model="row.key" placeholder="Header 名" style="flex:1" />
                <span class="env-eq">:</span>
                <t-input v-model="row.value" placeholder="Header 值" style="flex:1" />
                <t-button variant="text" theme="danger" shape="square" @click="removeHeader(i)">
                  <t-icon name="delete" />
                </t-button>
              </div>
              <t-button variant="outline" size="small" @click="addHeader">
                <template #icon><t-icon name="add" /></template>
                添加
              </t-button>
            </div>
          </div>
        </template>

        <div class="detail-foot">
          <div class="enable-box">
            <span class="lbl" style="margin:0">启用</span>
            <t-switch v-model="cur.enabled" />
          </div>
          <t-button theme="primary" :loading="saving" @click="save">保存</t-button>
        </div>
      </div>
    </section>

    <!-- 添加弹窗 -->
    <t-dialog
      v-model:visible="addVisible"
      header="添加 MCP"
      :width="520"
      :confirm-btn="{ content: '确认', disabled: !addForm.name }"
      :on-confirm="confirmAdd"
      dialogClassName="mcp-add-dialog"
    >
      <div class="add-form">
        <div class="field">
          <label class="lbl">名称</label>
          <t-input v-model="addForm.name" placeholder="输入名称" @enter="confirmAdd" />
        </div>
        <div class="field">
          <label class="lbl">类型</label>
          <t-select v-model="addForm.type" :options="typeOptions" />
        </div>
      </div>
    </t-dialog>

    <!-- 导入弹窗 -->
    <t-dialog
      v-model:visible="importVisible"
      header="导入 MCP 配置"
      :width="560"
      confirm-btn="导入"
      :on-confirm="confirmImport"
      dialogClassName="mcp-add-dialog"
    >
      <div class="field">
        <label class="lbl">粘贴 JSON 配置</label>
        <div class="sub">形如 <code>{ "mcpServers": { "name": { "command": "...", "args": [] } } }</code></div>
        <t-textarea v-model="importText" :autosize="{ minRows: 8, maxRows: 16 }" placeholder='{ "mcpServers": { } }' />
      </div>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, computed, watch, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botMcpApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const typeOptions = [
  { label: '本地命令 (Stdio)', value: 'stdio' },
  { label: 'SSE', value: 'sse' },
  { label: 'Streamable HTTP', value: 'http' }
]

const servers = ref([])
const cur = ref(null)
const keyword = ref('')
const saving = ref(false)

const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return servers.value
  return servers.value.filter(s => s.name.toLowerCase().includes(k))
})

async function load(selectId) {
  const res = await botMcpApi.list(props.botId)
  servers.value = res.servers || []
  if (selectId) cur.value = servers.value.find(s => s.id === selectId) || null
  else if (cur.value) cur.value = servers.value.find(s => s.id === cur.value.id) || null
}
function select(s) { cur.value = s }

/* 环境变量 / Header 的 key-value 双向映射 */
const envRows = ref([])
const headerRows = ref([])
watch(cur, (s) => {
  envRows.value = s ? Object.entries(s.env || {}).map(([key, value]) => ({ key, value })) : []
  headerRows.value = s ? Object.entries(s.headers || {}).map(([key, value]) => ({ key, value })) : []
}, { immediate: true })
function addEnv() { envRows.value.push({ key: '', value: '' }) }
function removeEnv(i) { envRows.value.splice(i, 1) }
function addHeader() { headerRows.value.push({ key: '', value: '' }) }
function removeHeader(i) { headerRows.value.splice(i, 1) }
function rowsToObj(rows) {
  const o = {}
  for (const r of rows) { if (r.key.trim()) o[r.key.trim()] = r.value }
  return o
}

async function save() {
  if (!cur.value.name.trim()) return MessagePlugin.warning('请填写名称')
  saving.value = true
  try {
    const payload = {
      name: cur.value.name.trim(),
      type: cur.value.type,
      command: cur.value.command,
      args: cur.value.args || [],
      env: rowsToObj(envRows.value),
      cwd: cur.value.cwd,
      url: cur.value.url,
      headers: rowsToObj(headerRows.value),
      enabled: cur.value.enabled
    }
    await botMcpApi.update(props.botId, cur.value.id, payload)
    await load(cur.value.id)
    MessagePlugin.success('已保存')
  } catch (e) {
    MessagePlugin.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

function remove() {
  const target = cur.value
  const dlg = DialogPlugin.confirm({
    header: '删除 MCP 服务器', body: `确认删除「${target.name}」？`, theme: 'warning',
    onConfirm: async () => {
      await botMcpApi.remove(props.botId, target.id)
      dlg.destroy()
      cur.value = null
      await load()
      MessagePlugin.success('已删除')
    }
  })
}

/* 添加 */
const addVisible = ref(false)
const addForm = reactive({ name: '', type: 'stdio' })
function openAdd() { addForm.name = ''; addForm.type = 'stdio'; addVisible.value = true }
async function confirmAdd() {
  if (!addForm.name.trim()) return MessagePlugin.warning('请填写名称')
  const created = await botMcpApi.create(props.botId, { name: addForm.name.trim(), type: addForm.type })
  addVisible.value = false
  await load(created.id)
  MessagePlugin.success('已添加')
}

/* 导入 */
const importVisible = ref(false)
const importText = ref('')
function openImport() { importText.value = ''; importVisible.value = true }
async function confirmImport() {
  let cfg
  try {
    cfg = JSON.parse(importText.value)
  } catch (e) {
    MessagePlugin.error('JSON 格式有误，请检查')
    return false
  }
  if (!cfg.mcpServers || typeof cfg.mcpServers !== 'object') {
    MessagePlugin.error('缺少 mcpServers 字段')
    return false
  }
  const res = await botMcpApi.import(props.botId, cfg)
  importVisible.value = false
  await load(res.servers?.[0]?.id)
  MessagePlugin.success(`已导入 ${res.servers?.length || 0} 个服务器`)
}

onMounted(load)
</script>

<style scoped>
.mcp-wrap { display: flex; height: 100%; min-height: 480px; margin: -4px 0; }

/* 左栏 */
.mcp-side {
  width: 260px; flex-shrink: 0; display: flex; flex-direction: column;
  border-right: 1px solid #f0f0f0; padding-right: 0;
}
.side-search { padding: 0 12px 12px 0; }
.side-list { flex: 1; overflow-y: auto; padding-right: 8px; display: flex; flex-direction: column; gap: 4px; }
.srv-item {
  display: flex; align-items: center; gap: 8px; padding: 10px 12px;
  border: 1px solid #ececec; border-radius: 10px; cursor: pointer; background: #fff;
}
.srv-item:hover { background: #fafafa; }
.srv-item.active { border-color: #d9d9d9; background: #f5f5f7; }
.dot { width: 8px; height: 8px; border-radius: 50%; background: #bbb; flex-shrink: 0; }
.dot.running { background: #00a870; }
.dot.error { background: #e34d59; }
.dot.draft { background: #ccc; }
.srv-name { flex: 1; min-width: 0; font-size: 14px; font-weight: 600; color: #1d1d1f; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.side-empty { color: #aaa; font-size: 13px; text-align: center; padding: 20px 0; }
.side-foot { display: flex; gap: 8px; padding: 12px 8px 0 0; border-top: 1px solid #f0f0f0; margin-top: 8px; }
.side-foot .t-button:first-child { flex: 1; }

/* 右栏 */
.mcp-main { flex: 1; min-width: 0; padding-left: 28px; overflow-y: auto; }

.empty {
  height: 100%; min-height: 420px; display: flex; flex-direction: column;
  align-items: center; justify-content: center; gap: 12px; color: #999;
}
.empty-icon {
  width: 56px; height: 56px; border-radius: 50%; background: #f5f5f7;
  display: flex; align-items: center; justify-content: center; color: #999;
}
.empty-title { font-size: 15px; font-weight: 600; color: #1d1d1f; }
.empty-desc { font-size: 13px; color: #999; }

/* 详情 */
.detail-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 20px; }
.dh-name { font-size: 18px; font-weight: 700; color: #1d1d1f; }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px 20px; }
.field { display: flex; flex-direction: column; margin-bottom: 16px; }
.lbl { font-size: 13px; font-weight: 600; color: #1d1d1f; margin-bottom: 8px; }
.sub { font-size: 12px; color: #999; margin: -2px 0 8px; }
.sub code { background: #f5f5f5; padding: 1px 4px; border-radius: 4px; }

.env-list { display: flex; flex-direction: column; gap: 8px; align-items: flex-start; }
.env-row { display: flex; align-items: center; gap: 8px; width: 100%; }
.env-eq { color: #999; }

.detail-foot {
  display: flex; align-items: center; justify-content: space-between;
  border-top: 1px solid #f0f0f0; padding-top: 16px; margin-top: 8px;
}
.enable-box { display: flex; align-items: center; gap: 10px; }

.add-form .field { margin-bottom: 16px; }
</style>

<!-- 弹窗在 body 下，scoped 命中不到 -->
<style>
.mcp-add-dialog.t-dialog { padding: 20px 20px 16px; }
.mcp-add-dialog .t-dialog__header { padding: 0; margin-bottom: 16px; }
.mcp-add-dialog .t-dialog__body { padding: 0; }
.mcp-add-dialog .t-dialog__footer { padding: 16px 0 0; }
</style>
