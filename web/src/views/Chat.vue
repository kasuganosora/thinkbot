<template>
  <div class="chat-page">
    <BotSidebar />
    <SessionList />
    <ChatWindow />
    <ChatToolPanel />
  </div>
</template>

<script setup>
import BotSidebar from '@/components/BotSidebar.vue'
import SessionList from '@/components/SessionList.vue'
import ChatWindow from '@/components/ChatWindow.vue'
import ChatToolPanel from '@/components/ChatToolPanel.vue'
</script>

<style scoped>
.chat-page {
  display: flex;
  height: 100%;
  width: 100%;
}
</style>
