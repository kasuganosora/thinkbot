<template>
  <div class="login-page" data-testid="login-page">
    <div class="login-bg"></div>
    <div class="login-card">
      <div class="brand">
        <div class="brand-logo">🤖</div>
        <h1 class="brand-title">Bot 平台</h1>
        <p class="brand-sub">智能对话 · 多 Bot 管理 · 一站式工作台</p>
      </div>

      <t-form :data="form" :rules="rules" ref="formRef" @submit="onSubmit" label-width="0" data-testid="login-form">
        <t-form-item name="username">
          <t-input v-model="form.username" size="large" placeholder="请输入用户名" clearable data-testid="login-username">
            <template #prefix-icon><t-icon name="user" /></template>
          </t-input>
        </t-form-item>
        <t-form-item name="password">
          <t-input
            v-model="form.password"
            type="password"
            size="large"
            placeholder="请输入密码"
            clearable
            data-testid="login-password"
          >
            <template #prefix-icon><t-icon name="lock-on" /></template>
          </t-input>
        </t-form-item>

        <div class="login-options">
          <t-checkbox v-model="remember">记住我</t-checkbox>
          <t-link theme="primary" hover="color">忘记密码？</t-link>
        </div>

        <t-button theme="primary" type="submit" size="large" block :loading="loading" data-testid="login-submit">
          登 录
        </t-button>

        <div class="login-tip">
          演示账号：任意用户名 + 密码即可登录
        </div>
      </t-form>
    </div>
  </div>
</template>

<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { MessagePlugin } from 'tdesign-vue-next'
import { useUserStore } from '@/stores/user'
import { authApi } from '@/api/services'

const router = useRouter()
const userStore = useUserStore()

const formRef = ref()
const loading = ref(false)
const remember = ref(true)
const form = ref({ username: '', password: '' })

const rules = {
  username: [{ required: true, message: '请输入用户名', type: 'error' }],
  password: [{ required: true, message: '请输入密码', type: 'error' }]
}

function onSubmit({ validateResult }) {
  if (validateResult !== true) return
  loading.value = true
  authApi.login(form.value.username, form.value.password)
    .then((resp) => {
      userStore.login({
        id: resp.id,
        username: resp.username,
        nickname: resp.displayName || resp.username,
        displayName: resp.displayName || resp.username,
        role: resp.role,
        avatar: resp.avatar || '',
        email: `${resp.username}@thinkbot.dev`,
        bio: '这个人很懒，什么都没留下'
      })
      MessagePlugin.success('登录成功')
      router.push({ name: 'chat' })
    })
    .catch((e) => {
      MessagePlugin.error(e.message || '登录失败')
    })
    .finally(() => { loading.value = false })
}
</script>

<style scoped>
.login-page {
  position: relative;
  height: 100%;
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
}
.login-bg {
  position: absolute;
  inset: 0;
  background: linear-gradient(135deg, #0b1020 0%, #1b2440 45%, #0d2818 100%);
}
.login-bg::after {
  content: '';
  position: absolute;
  inset: 0;
  background:
    radial-gradient(circle at 20% 20%, rgba(0, 168, 112, 0.25), transparent 40%),
    radial-gradient(circle at 80% 70%, rgba(64, 128, 255, 0.25), transparent 40%);
}
.login-card {
  position: relative;
  z-index: 1;
  width: 400px;
  padding: 40px 36px;
  background: rgba(255, 255, 255, 0.97);
  border-radius: 20px;
  box-shadow: 0 24px 60px rgba(0, 0, 0, 0.35);
}
.brand {
  text-align: center;
  margin-bottom: 28px;
}
.brand-logo {
  font-size: 48px;
  line-height: 1;
}
.brand-title {
  margin-top: 12px;
  font-size: 26px;
  font-weight: 700;
}
.brand-sub {
  margin-top: 8px;
  font-size: 13px;
  color: #888;
}
.login-options {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 18px;
}
.login-tip {
  margin-top: 16px;
  text-align: center;
  font-size: 12px;
  color: #aaa;
}
</style>
