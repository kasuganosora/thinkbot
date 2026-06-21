import { ref } from 'vue'
import { chatApi } from '@/api/client'
import { streamChat } from '@/api/sse'
import type { ChatMessage, HistoryPage, ToolCallInfo } from '@/types/api'

const bots = ref<{ id: string; name: string; running: boolean }[]>([])
const activeBotId = ref<string | null>(null)
const messages = ref<ChatMessage[]>([])
const nextCursor = ref<string | null>(null)
const loadingHistory = ref(false)
const sending = ref(false)
const streamingText = ref('')
const status = ref<'idle' | 'streaming' | 'error'>('idle')
const toolCalls = ref<ToolCallInfo[]>([])

let abortController: AbortController | null = null
let idCounter = -1

export function useChat() {
  async function loadBots() {
    bots.value = await chatApi.bots()
    if (!activeBotId.value && bots.value.length > 0) {
      selectBot(bots.value[0].id)
    }
  }

  function selectBot(botId: string) {
    if (activeBotId.value === botId) return
    activeBotId.value = botId
    messages.value = []
    nextCursor.value = null
    loadHistory()
  }

  async function loadHistory() {
    if (!activeBotId.value || loadingHistory.value) return
    loadingHistory.value = true
    try {
      const page: HistoryPage = await chatApi.history(activeBotId.value, nextCursor.value ?? undefined)
      messages.value = [...page.messages.reverse(), ...messages.value]
      nextCursor.value = page.nextCursor
    } finally {
      loadingHistory.value = false
    }
  }

  async function sendMessage(text: string) {
    if (!activeBotId.value || sending.value) return
    const botId = activeBotId.value

    messages.value.push({
      id: idCounter--, botId, userId: '', role: 'user',
      content: text, traceId: '', createdAt: new Date().toISOString(),
    })

    sending.value = true
    streamingText.value = ''
    status.value = 'streaming'
    toolCalls.value = []
    abortController = new AbortController()

    await streamChat(botId, text, (event) => {
      switch (event.type) {
        case 'text_delta':
          streamingText.value += (event.data.text as string) ?? ''
          break
        case 'tool_call':
          toolCalls.value.push({
            tool: (event.data.tool as string) ?? '',
            input: event.data.input,
            status: 'calling',
          })
          break
        case 'tool_result':
          toolCalls.value.push({
            tool: (event.data.tool as string) ?? '',
            input: null,
            status: event.data.error ? 'error' : 'done',
            output: event.data.output,
            error: event.data.error as string | undefined,
          })
          break
        case 'done':
          messages.value.push({
            id: idCounter--, botId, userId: '', role: 'assistant',
            content: (event.data.text as string) ?? streamingText.value,
            traceId: '', createdAt: new Date().toISOString(),
          })
          streamingText.value = ''
          toolCalls.value = []
          sending.value = false
          status.value = 'idle'
          break
        case 'error':
          status.value = 'error'
          sending.value = false
          break
      }
    }, abortController.signal)
  }

  function stopStreaming() {
    abortController?.abort()
    abortController = null
    if (streamingText.value) {
      messages.value.push({
        id: idCounter--, botId: activeBotId.value ?? '', userId: '', role: 'assistant',
        content: streamingText.value, traceId: '', createdAt: new Date().toISOString(),
      })
    }
    streamingText.value = ''
    sending.value = false
    status.value = 'idle'
  }

  return {
    bots, activeBotId, messages, nextCursor, loadingHistory,
    sending, streamingText, status, toolCalls,
    loadBots, selectBot, loadHistory, sendMessage, stopStreaming,
  }
}
