<template>
  <SettingsShell title="用户设置">
    <t-card title="个人资料" :bordered="false" class="card">
      <div class="avatar-row">
        <t-avatar size="64px" :style="{ background: '#00a870', color: '#fff', fontSize: '28px' }">
          {{ (form.nickname || 'U').charAt(0).toUpperCase() }}
        </t-avatar>
        <div class="avatar-tip">
          <div class="avatar-name">{{ form.nickname }}</div>
          <t-button size="small" variant="outline">更换头像</t-button>
        </div>
      </div>

      <t-form :data="form" label-align="top" class="form">
        <t-form-item label="用户名">
          <t-input v-model="form.username" disabled />
        </t-form-item>
        <t-form-item label="昵称">
          <t-input v-model="form.nickname" placeholder="请输入昵称" />
        </t-form-item>
        <t-form-item label="邮箱">
          <t-input v-model="form.email" placeholder="请输入邮箱" />
        </t-form-item>
        <t-form-item label="个人简介">
          <t-textarea v-model="form.bio" :autosize="{ minRows: 3 }" placeholder="介绍一下自己" />
        </t-form-item>
      </t-form>
    </t-card>

    <t-card title="安全设置" :bordered="false" class="card">
      <t-form label-align="top">
        <t-form-item label="当前密码">
          <t-input type="password" v-model="pwd.old" placeholder="修改密码时填写" data-testid="user-old-password" style="width: 320px" />
        </t-form-item>
        <t-form-item label="新密码">
          <t-input type="password" v-model="pwd.new" placeholder="至少 6 位，留空表示不修改" data-testid="user-new-password" style="width: 320px" />
        </t-form-item>
        <t-button variant="outline" @click="changePassword" data-testid="user-change-pwd-btn">修改密码</t-button>
      </t-form>
    </t-card>

    <t-card title="生成授权码" :bordered="false" class="card">
      <t-button
        theme="primary"
        :loading="generating"
        data-testid="bind-generate-btn"
        @click="generate"
      >生成授权码</t-button>
      <t-alert
        v-if="generated.code"
        theme="success"
        class="gen-alert"
        data-testid="bind-generated"
      >
        <template #message>
          <div class="gen-code">授权码：<strong>{{ generated.code }}</strong></div>
          <div class="gen-hint">{{ generated.hint }}</div>
        </template>
      </t-alert>
    </t-card>

    <t-card title="已绑定身份" :bordered="false" class="card">
      <t-table
        row-key="id"
        data-testid="bind-bindings-table"
        :data="bindings"
        :columns="bindingColumns"
        :loading="bindingsLoading"
        size="small"
        hover
      >
        <template #createdAt="{ row }">{{ formatTime(row.createdAt) }}</template>
        <template #op="{ row }">
          <t-popconfirm content="确认删除该绑定？" @confirm="() => removeBinding(row)">
            <t-button
              variant="text"
              theme="danger"
              size="small"
              :data-testid="`bind-remove-btn-${row.id}`"
            >删除</t-button>
          </t-popconfirm>
        </template>
      </t-table>
    </t-card>

    <t-card title="未使用的授权码" :bordered="false" class="card">
      <t-table
        row-key="code"
        data-testid="bind-codes-table"
        :data="codes"
        :columns="codeColumns"
        :loading="codesLoading"
        size="small"
        hover
      >
        <template #expiresAt="{ row }">{{ formatTime(row.expiresAt) }}</template>
        <template #ttlSec="{ row }">{{ row.ttlSec }} 秒</template>
      </t-table>
    </t-card>

    <div class="footer-actions">
      <t-button theme="primary" @click="save" data-testid="user-save-btn">保存修改</t-button>
      <t-button variant="outline" @click="reset">重置</t-button>
    </div>
  </SettingsShell>
</template>

<script setup>
import { ref } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import SettingsShell from '@/components/SettingsShell.vue'
import { useUserStore } from '@/stores/user'
import { authApi, userApi, bindApi } from '@/api/services'
import { onMounted } from 'vue'

const userStore = useUserStore()
const pwd = ref({ old: '', new: '' })

function buildForm() {
  return {
    username: userStore.user?.username || '',
    nickname: userStore.user?.nickname || userStore.user?.displayName || '',
    email: userStore.user?.email || '',
    bio: userStore.user?.bio || ''
  }
}
const form = ref(buildForm())

async function save() {
  // 同步后端（mock）：更新当前用户资料
  try {
    const uid = userStore.user?.id
    if (uid) await userApi.update(uid, { displayName: form.value.nickname, email: form.value.email })
  } catch (e) { /* mock 下忽略 */ }
  userStore.updateProfile({
    nickname: form.value.nickname,
    displayName: form.value.nickname,
    email: form.value.email,
    bio: form.value.bio
  })
  MessagePlugin.success('已保存')
}

async function changePassword() {
  if (!pwd.value.new) return MessagePlugin.warning('请输入新密码')
  if (pwd.value.new.length < 6) return MessagePlugin.warning('新密码至少 6 位')
  try {
    await authApi.changePassword(pwd.value.old, pwd.value.new)
    pwd.value = { old: '', new: '' }
    MessagePlugin.success('密码已修改')
  } catch (e) {
    MessagePlugin.error(e.message || '修改失败')
  }
}

function reset() {
  form.value = buildForm()
  pwd.value = { old: '', new: '' }
}

/* ---------------- 身份绑定 ---------------- */
const generating = ref(false)
const generated = ref({})
const bindingsLoading = ref(false)
const bindings = ref([])
const codesLoading = ref(false)
const codes = ref([])

const bindingColumns = [
  { colKey: 'id', title: 'ID', width: 120 },
  { colKey: 'platform', title: '平台', width: 120 },
  { colKey: 'platformUserId', title: '平台用户 ID' },
  { colKey: 'createdAt', title: '绑定时间', width: 180 },
  { colKey: 'op', title: '操作', width: 90, fixed: 'right' }
]

const codeColumns = [
  { colKey: 'code', title: '授权码', width: 200 },
  { colKey: 'expiresAt', title: '过期时间', width: 180 },
  { colKey: 'ttlSec', title: '剩余有效期' }
]

function formatTime(iso) {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return String(iso)
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

async function generate() {
  generating.value = true
  try {
    generated.value = await bindApi.generate()
    MessagePlugin.success('授权码已生成')
    await loadCodes()
  } catch (e) {
    MessagePlugin.error(`生成授权码失败：${e.message || e}`)
  } finally {
    generating.value = false
  }
}

async function loadBindings() {
  bindingsLoading.value = true
  try {
    const res = await bindApi.listBindings()
    bindings.value = res.bindings || []
  } catch (e) {
    MessagePlugin.error(`加载绑定列表失败：${e.message || e}`)
  } finally {
    bindingsLoading.value = false
  }
}

async function loadCodes() {
  codesLoading.value = true
  try {
    const res = await bindApi.listCodes()
    codes.value = res.codes || []
  } catch (e) {
    MessagePlugin.error(`加载授权码列表失败：${e.message || e}`)
  } finally {
    codesLoading.value = false
  }
}

async function removeBinding(row) {
  try {
    await bindApi.removeBinding(row.id)
    MessagePlugin.success('已删除绑定')
    await loadBindings()
  } catch (e) {
    MessagePlugin.error(`删除绑定失败：${e.message || e}`)
  }
}

onMounted(() => {
  loadBindings()
  loadCodes()
})
</script>

<style scoped>
.card {
  margin-bottom: 20px;
}
.avatar-row {
  display: flex;
  align-items: center;
  gap: 16px;
  margin-bottom: 12px;
}
.avatar-name {
  font-size: 16px;
  font-weight: 600;
  margin-bottom: 8px;
}
.form {
  margin-top: 8px;
}
.footer-actions {
  display: flex;
  gap: 12px;
}
.gen-alert {
  margin-top: 16px;
}
.gen-code {
  font-size: 15px;
  margin-bottom: 4px;
}
.gen-hint {
  color: #8a8a8a;
  font-size: 13px;
}
</style>
