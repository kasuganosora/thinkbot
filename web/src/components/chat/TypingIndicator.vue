<script setup lang="ts">
const dots = [0, 1, 2]
</script>

<template>
  <div class="typing">
    <div class="avatar">
      <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2">
        <rect x="3" y="11" width="18" height="10" rx="2" /><circle cx="12" cy="5" r="2" /><path d="M12 7v4" />
      </svg>
    </div>
    <div class="dots-bubble">
      <span v-for="i in dots" :key="i" class="dot" :style="{ animationDelay: `${i * 0.15}s` }" />
    </div>
  </div>
</template>

<style scoped>
.typing { display: flex; gap: 0.75rem; align-items: flex-start; max-width: 780px; }
.avatar { flex-shrink: 0; width: 32px; height: 32px; border-radius: var(--radius-sm); background: var(--accent); color: var(--muted-foreground); display: flex; align-items: center; justify-content: center; }
.dots-bubble { display: flex; gap: 5px; align-items: center; padding: 0.75rem 1rem; background: var(--card); border: 1px solid var(--border); border-radius: var(--radius-md); border-bottom-left-radius: var(--radius-sm); }
.dot { width: 7px; height: 7px; border-radius: 50%; background: var(--muted-foreground); animation: typing-dot 1.4s ease-in-out infinite; }
</style>
