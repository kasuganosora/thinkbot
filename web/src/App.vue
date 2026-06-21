<script setup lang="ts">
import { onMounted } from 'vue'
import { useAuth } from '@/composables/useAuth'
import { useChat } from '@/composables/useChat'
import LoginView from '@/views/LoginView.vue'
import AppLayout from '@/layouts/AppLayout.vue'
import { TToastContainer } from '@/components/ui'

const { user, loading, checkSession } = useAuth()
const { loadBots } = useChat()

onMounted(async () => {
  const ok = await checkSession()
  if (ok) await loadBots()
})
</script>

<template>
  <div v-if="loading" class="app-boot">
    <div class="boot-spinner" />
  </div>
  <LoginView v-else-if="!user" />
  <AppLayout v-else />
  <TToastContainer />
</template>

<style scoped>
.app-boot {
  display: flex;
  align-items: center;
  justify-content: center;
  height: 100%;
}
.boot-spinner {
  width: 32px;
  height: 32px;
  border: 2.5px solid var(--border);
  border-top-color: var(--muted-foreground);
  border-radius: 50%;
  animation: spin 0.6s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
</style>
