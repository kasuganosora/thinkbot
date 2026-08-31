<template>
  <div class="skills-wrap" data-testid="bot-skills">
    <!-- 头部 -->
    <div class="sk-head">
      <span class="sk-title">技能</span>
      <div class="sk-actions">
        <span class="sk-path-link" @click="showPath">
          <t-icon name="control-platform" /> 技能路径
        </span>
        <t-button theme="primary" @click="openCreate">
          <template #icon><t-icon name="add" /></template>
          新建技能
        </t-button>
      </div>
    </div>

    <!-- 空态 -->
    <t-loading :loading="loading" size="small">
      <div v-if="!loading && skills.length === 0" class="sk-empty">
        <t-icon name="extension" size="28px" />
        <div class="e-title">暂无技能</div>
        <div class="e-desc">新建技能以扩展 Bot 的能力</div>
      </div>

      <!-- 卡片网格 -->
      <div v-else class="sk-grid">
        <div v-for="s in skills" :key="s.id" class="sk-card">
          <div class="card-top">
            <span class="card-name">{{ s.name }}</span>
            <div class="card-ops">
              <t-icon name="edit" @click="openEdit(s)" />
              <t-icon name="browse" @click="openPreview(s)" />
              <t-icon name="delete" class="op-del" @click="remove(s)" />
            </div>
          </div>
          <div class="card-desc">{{ s.description || '（无描述）' }}</div>
          <div class="card-tags">
            <span class="tag">{{ s.source === 'managed' ? '托管' : '本地' }}</span>
            <span class="tag">{{ s.status === 'active' ? '生效中' : '未生效' }}</span>
          </div>
          <div class="card-path">{{ s.path }}</div>
        </div>
      </div>
    </t-loading>

    <!-- 新建 / 编辑 弹窗 -->
    <t-dialog
      v-model:visible="editorVisible"
      :header="editing ? '编辑技能' : '新建技能'"
      :width="900"
      :footer="false"
      dialogClassName="skill-editor-dialog"
    >
      <div class="editor">
        <div class="editor-box">
          <div class="ln-col" ref="lnCol">
            <div v-for="n in lineCount" :key="n" class="ln">{{ n }}</div>
          </div>
          <textarea
            ref="taRef"
            v-model="draft"
            class="code-ta"
            spellcheck="false"
            wrap="off"
            @scroll="syncScroll"
            @keydown.tab.prevent="onTab"
          ></textarea>
        </div>
      </div>
      <div class="editor-foot">
        <t-button variant="outline" @click="editorVisible = false">取消</t-button>
        <t-button theme="primary" :loading="saving" @click="saveSkill">保存</t-button>
      </div>
    </t-dialog>

    <!-- 预览弹窗 -->
    <t-dialog
      v-model:visible="previewVisible"
      :header="`预览：${preview?.name || ''}`"
      :width="820"
      :footer="false"
      dialogClassName="skill-editor-dialog"
    >
      <pre class="preview-pre">{{ preview?.content }}</pre>
      <div class="editor-foot">
        <t-button variant="outline" @click="previewVisible = false">关闭</t-button>
        <t-button theme="primary" @click="editFromPreview">编辑</t-button>
      </div>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, computed, nextTick } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botSkillApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const skills = ref([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    const res = await botSkillApi.list(props.botId)
    skills.value = res.skills || []
  } finally {
    loading.value = false
  }
}
load()

/* ---------------- 编辑器 ---------------- */
const editorVisible = ref(false)
const editing = ref(null) // 正在编辑的技能对象；null 表示新建
const draft = ref('')
const saving = ref(false)
const taRef = ref(null)
const lnCol = ref(null)

const lineCount = computed(() => Math.max(draft.value.split('\n').length, 1))

function openCreate() {
  editing.value = null
  draft.value = botSkillApi.template()
  editorVisible.value = true
}
function openEdit(s) {
  editing.value = s
  draft.value = s.content
  editorVisible.value = true
}
function syncScroll(e) {
  if (lnCol.value) lnCol.value.scrollTop = e.target.scrollTop
}
function onTab(e) {
  const el = e.target
  const start = el.selectionStart, end = el.selectionEnd
  draft.value = draft.value.slice(0, start) + '  ' + draft.value.slice(end)
  nextTick(() => { el.selectionStart = el.selectionEnd = start + 2 })
}

async function saveSkill() {
  if (!draft.value.trim()) return MessagePlugin.warning('内容不能为空')
  saving.value = true
  try {
    if (editing.value) {
      await botSkillApi.update(props.botId, editing.value.id, draft.value)
      MessagePlugin.success('已保存')
    } else {
      await botSkillApi.create(props.botId, draft.value)
      MessagePlugin.success('已创建')
    }
    editorVisible.value = false
    await load()
  } catch (e) {
    MessagePlugin.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

/* ---------------- 预览 ---------------- */
const previewVisible = ref(false)
const preview = ref(null)
function openPreview(s) { preview.value = s; previewVisible.value = true }
function editFromPreview() {
  previewVisible.value = false
  openEdit(preview.value)
}

/* ---------------- 删除 ---------------- */
function remove(s) {
  const dlg = DialogPlugin.confirm({
    header: '删除技能', body: `确认删除「${s.name}」？该操作不可恢复。`, theme: 'warning',
    onConfirm: async () => {
      await botSkillApi.remove(props.botId, s.id)
      dlg.destroy()
      MessagePlugin.success('已删除')
      load()
    }
  })
}

function showPath() {
  MessagePlugin.info('技能根目录：/data/skills/')
}
</script>

<style scoped>
.skills-wrap { width: 100%; }

.sk-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 20px; }
.sk-title { font-size: 16px; font-weight: 600; color: #1d1d1f; }
.sk-actions { display: flex; align-items: center; gap: 16px; }
.sk-path-link {
  display: inline-flex; align-items: center; gap: 4px; font-size: 13px;
  color: #666; cursor: pointer;
}
.sk-path-link:hover { color: #1d1d1f; }

.sk-empty {
  display: flex; flex-direction: column; align-items: center; justify-content: center;
  gap: 8px; padding: 80px 0; color: #aaa;
}
.e-title { font-size: 15px; font-weight: 600; color: #1d1d1f; }
.e-desc { font-size: 13px; color: #999; }

.sk-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(360px, 1fr)); gap: 18px; }
.sk-card {
  border: 1px solid #ececec; border-radius: 14px; padding: 18px 18px 16px;
  display: flex; flex-direction: column; gap: 10px; background: #fff;
}
.sk-card:hover { border-color: #ddd; box-shadow: 0 2px 10px rgba(0,0,0,0.04); }
.card-top { display: flex; align-items: flex-start; justify-content: space-between; }
.card-name { font-size: 16px; font-weight: 700; color: #1d1d1f; }
.card-ops { display: flex; align-items: center; gap: 12px; color: #999; }
.card-ops .t-icon { cursor: pointer; }
.card-ops .t-icon:hover { color: #1d1d1f; }
.card-ops .op-del:hover { color: #e34d59; }
.card-desc {
  font-size: 13px; color: #666; line-height: 1.5; min-height: 39px;
  display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden;
}
.card-tags { display: flex; gap: 8px; }
.tag { font-size: 12px; padding: 3px 12px; border-radius: 20px; background: #f2f3f5; color: #666; }
.card-path { font-size: 12px; color: #aaa; word-break: break-all; }

/* 编辑器 */
.editor-box {
  display: flex; border: 1px solid #eee; border-radius: 10px; overflow: hidden;
  background: #fff; height: 60vh; min-height: 380px;
}
.ln-col {
  flex-shrink: 0; width: 52px; background: #fafafa; border-right: 1px solid #f0f0f0;
  overflow: hidden; padding: 12px 0; text-align: right;
}
.ln {
  font-family: 'SF Mono', Menlo, Consolas, monospace; font-size: 13px; line-height: 1.7;
  color: #bbb; padding-right: 12px;
}
.code-ta {
  flex: 1; border: none; outline: none; resize: none; padding: 12px 14px;
  font-family: 'SF Mono', Menlo, Consolas, monospace; font-size: 13px; line-height: 1.7;
  color: #1d1d1f; white-space: pre; overflow: auto;
}
.editor-foot { display: flex; justify-content: flex-end; gap: 12px; margin-top: 16px; }

.preview-pre {
  background: #fafafa; border: 1px solid #f0f0f0; border-radius: 10px;
  padding: 16px; font-family: 'SF Mono', Menlo, Consolas, monospace; font-size: 13px;
  line-height: 1.7; color: #1d1d1f; max-height: 60vh; overflow: auto; white-space: pre-wrap;
}
</style>

<style>
.skill-editor-dialog.t-dialog { padding: 20px 24px 20px; }
.skill-editor-dialog .t-dialog__body { padding: 12px 0 0; }
</style>
