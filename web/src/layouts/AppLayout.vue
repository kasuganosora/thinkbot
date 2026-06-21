<script setup lang="ts">
import AppSidebar from '@/components/AppSidebar.vue'
</script>

<template>
  <div class="layout">
    <AppSidebar />
    <main class="main-pane">
      <RouterView />
    </main>
  </div>
</template>

<style scoped>
.layout {
  display: flex;
  height: 100%;
}
.main-pane {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}
</style>
