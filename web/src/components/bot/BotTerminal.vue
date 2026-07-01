<template>
  <XtermConsole
    :connect="onConnect"
    :exec="onExec"
    title="终端"
    body-height="440px"
  >
    <template #tip>
      这是容器 shell 的模拟终端（mock 数据）。输入 <code>help</code> 查看可用命令。
    </template>
  </XtermConsole>
</template>

<script setup>
import XtermConsole from '@/components/common/XtermConsole.vue'
import { botTerminalApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

function onConnect() { return botTerminalApi.connect(props.botId) }
function onExec(cmd) { return botTerminalApi.exec(props.botId, cmd) }
</script>
