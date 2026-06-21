import { ref, readonly, computed } from 'vue'
import { authApi, AuthError } from '@/api/client'
import type { UserInfo } from '@/types/api'

const user = ref<UserInfo | null>(null)
const loading = ref(false)

export function useAuth() {
  const isAdmin = computed(() => user.value?.role === 'admin')

  async function checkSession() {
    loading.value = true
    try {
      user.value = await authApi.me()
      return true
    } catch (e) {
      if (e instanceof AuthError) {
        user.value = null
      }
      return false
    } finally {
      loading.value = false
    }
  }

  async function login(username: string, password: string) {
    loading.value = true
    try {
      user.value = await authApi.login(username, password)
    } finally {
      loading.value = false
    }
  }

  async function logout() {
    try {
      await authApi.logout()
    } finally {
      user.value = null
    }
  }

  return {
    user: readonly(user),
    loading: readonly(loading),
    isAdmin,
    checkSession,
    login,
    logout,
  }
}
