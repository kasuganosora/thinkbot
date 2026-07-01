import { defineStore } from 'pinia'
import { ref, computed } from 'vue'

export const useUserStore = defineStore('user', () => {
  const user = ref(JSON.parse(localStorage.getItem('bp_user') || 'null'))

  const isLoggedIn = computed(() => !!user.value)

  function login(profile) {
    user.value = profile
    localStorage.setItem('bp_user', JSON.stringify(profile))
  }

  function logout() {
    user.value = null
    localStorage.removeItem('bp_user')
  }

  function updateProfile(patch) {
    user.value = { ...user.value, ...patch }
    localStorage.setItem('bp_user', JSON.stringify(user.value))
  }

  return { user, isLoggedIn, login, logout, updateProfile }
})
