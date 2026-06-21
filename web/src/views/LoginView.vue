<script setup lang="ts">
import { ref } from 'vue'
import { Sun, Moon, Bot } from 'lucide-vue-next'
import { useAuth } from '@/composables/useAuth'
import { useDarkMode } from '@/composables/useDarkMode'
import { TButton, TInput } from '@/components/ui'

const { login } = useAuth()
const { theme, toggle } = useDarkMode()

const username = ref('')
const password = ref('')
const error = ref('')
const submitting = ref(false)

async function handleSubmit() {
  if (!username.value || !password.value) {
    error.value = '请输入用户名和密码'
    return
  }
  error.value = ''
  submitting.value = true
  try {
    await login(username.value, password.value)
  } catch (e) {
    error.value = e instanceof Error ? e.message : '登录失败'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <TButton variant="ghost" size="icon" class="theme-btn" @click="toggle">
      <Sun v-if="theme === 'dark'" :size="18" />
      <Moon v-else :size="18" />
    </TButton>

    <div class="login-card">
      <div class="login-header">
        <div class="logo">
          <Bot :size="32" />
        </div>
        <h1>ThinkBot</h1>
        <p>智能对话平台</p>
      </div>

      <form @submit.prevent="handleSubmit">
        <div class="form-field">
          <label>用户名</label>
          <TInput
            v-model="username"
            placeholder="输入用户名"
            :disabled="submitting"
          />
        </div>

        <div class="form-field">
          <label>密码</label>
          <TInput
            v-model="password"
            type="password"
            placeholder="输入密码"
            :disabled="submitting"
          />
        </div>

        <p v-if="error" class="error-msg">{{ error }}</p>

        <TButton
          type="submit"
          :loading="submitting"
          size="lg"
          class="login-submit"
        >
          {{ submitting ? '登录中…' : '登 录' }}
        </TButton>
      </form>
    </div>
  </div>
</template>

<style scoped>
@import '@/assets/page-common.css';

.login-page {
  display: flex;
  align-items: center;
  justify-content: center;
  height: 100%;
  background: var(--background);
  position: relative;
}

.theme-btn {
  position: absolute !important;
  top: 1.5rem;
  right: 1.5rem;
  color: var(--muted-foreground);
}

.login-card {
  width: 100%;
  max-width: 380px;
  padding: 2.5rem;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: var(--radius-xl);
  box-shadow: var(--shadow-lg);
}

.login-header {
  text-align: center;
  margin-bottom: 2rem;
}

.logo {
  width: 64px;
  height: 64px;
  margin: 0 auto 1rem;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--foreground);
  color: var(--background);
  border-radius: var(--radius-lg);
}

.login-header h1 {
  font-size: 1.5rem;
  font-weight: 700;
  letter-spacing: -0.02em;
}

.login-header p {
  margin-top: 0.25rem;
  color: var(--muted-foreground);
  font-size: 0.8125rem;
}

.error-msg {
  color: var(--destructive);
  font-size: 0.8125rem;
  margin-bottom: 1rem;
}

.login-submit {
  width: 100%;
}
</style>
