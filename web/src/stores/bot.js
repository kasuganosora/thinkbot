import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { chatApi } from '@/api/services'

let idSeed = 1000
const uid = () => `id_${++idSeed}`

function seedData() {
  return [
    {
      id: 'bot_1',
      name: '元宝助手',
      avatar: '🤖',
      desc: '全能 AI 助手，擅长问答、写作与决策评估',
      model: 'gpt-4o',
      temperature: 0.7,
      prompt: '你是一个乐于助人的 AI 助手。',
      sessions: [
        {
          id: 'sess_1',
          title: '深圳交管12123查询科目一日期',
          updatedAt: Date.now() - 3600_000,
          messages: [
            { id: uid(), role: 'user', content: '帮我查询科目一考试日期怎么操作？' },
            { id: uid(), role: 'assistant', content: '你可以打开「交管12123」App，进入「考试预约」→「我的预约」即可查看科目一的考试日期与考场信息。' }
          ]
        },
        {
          id: 'sess_2',
          title: 'MySQL 5.7 查看表索引方法',
          updatedAt: Date.now() - 7200_000,
          messages: [
            { id: uid(), role: 'user', content: 'MySQL 5.7 怎么查看一张表的索引？' },
            { id: uid(), role: 'assistant', content: '可以使用：`SHOW INDEX FROM 表名;` 或查询 `information_schema.STATISTICS` 表。' }
          ]
        },
        {
          id: 'sess_3',
          title: '优化简化职效自评文案',
          updatedAt: Date.now() - 86400_000,
          messages: []
        }
      ]
    },
    {
      id: 'bot_2',
      name: '代码专家',
      avatar: '💻',
      desc: '专注于代码生成、Review 与重构',
      model: 'claude-3.5-sonnet',
      temperature: 0.3,
      prompt: '你是一名资深软件工程师，回答务必准确、给出可运行代码。',
      sessions: [
        {
          id: 'sess_4',
          title: '接口与 Clojure 映射整理',
          updatedAt: Date.now() - 5000_000,
          messages: []
        }
      ]
    },
    {
      id: 'bot_3',
      name: '文案写手',
      avatar: '✍️',
      desc: '营销文案、公众号、商务沟通',
      model: 'gpt-4o-mini',
      temperature: 0.9,
      prompt: '你是一名专业文案策划。',
      sessions: []
    }
  ]
}

export const useBotStore = defineStore('bot', () => {
  const bots = ref(JSON.parse(localStorage.getItem('bp_bots') || 'null') || seedData())
  const activeBotId = ref(bots.value[0]?.id || '')
  const activeSessionId = ref(bots.value[0]?.sessions[0]?.id || '')

  function persist() {
    localStorage.setItem('bp_bots', JSON.stringify(bots.value))
  }

  const activeBot = computed(() => bots.value.find(b => b.id === activeBotId.value))
  const sessions = computed(() => activeBot.value?.sessions || [])
  const activeSession = computed(() => sessions.value.find(s => s.id === activeSessionId.value))

  function selectBot(id) {
    activeBotId.value = id
    const bot = bots.value.find(b => b.id === id)
    activeSessionId.value = bot?.sessions[0]?.id || ''
  }

  function selectSession(id) {
    activeSessionId.value = id
  }

  function createBot(payload = {}) {
    const bot = {
      id: uid(),
      name: payload.name || '新建 Bot',
      avatar: payload.avatar || '🆕',
      avatarUrl: payload.avatarUrl || '',
      desc: payload.desc || '请编辑 Bot 描述',
      model: payload.model || 'gpt-4o',
      temperature: 0.7,
      prompt: '',
      timezone: payload.timezone || '',
      securityPolicy: payload.securityPolicy || 'allow_all',
      running: false,
      status: 'stopped',
      createdAt: Date.now(),
      sessions: []
    }
    bots.value.push(bot)
    persist()
    return bot
  }

  function updateBot(id, patch) {
    const bot = bots.value.find(b => b.id === id)
    if (bot) Object.assign(bot, patch)
    persist()
  }

  function deleteBot(id) {
    const idx = bots.value.findIndex(b => b.id === id)
    if (idx > -1) bots.value.splice(idx, 1)
    if (activeBotId.value === id) selectBot(bots.value[0]?.id || '')
    persist()
  }

  function createSession() {
    if (!activeBot.value) return
    const sess = {
      id: uid(),
      title: '新会话',
      updatedAt: Date.now(),
      messages: []
    }
    activeBot.value.sessions.unshift(sess)
    activeSessionId.value = sess.id
    persist()
  }

  function deleteSession(id) {
    const list = activeBot.value?.sessions
    if (!list) return
    const idx = list.findIndex(s => s.id === id)
    if (idx > -1) list.splice(idx, 1)
    if (activeSessionId.value === id) activeSessionId.value = list[0]?.id || ''
    persist()
  }

  function sendMessage(content) {
    if (!activeSession.value) {
      createSession()
    }
    const sess = activeSession.value
    sess.messages.push({ id: uid(), role: 'user', content })
    if (sess.messages.length === 1) {
      sess.title = content.slice(0, 18)
    }
    sess.updatedAt = Date.now()
    persist()
    // 通过 API（mock）获取回复，对齐后端 chat/send
    const botId = activeBot.value?.id
    chatApi.send(botId, content)
      .then((resp) => {
        sess.messages.push({ id: uid(), role: 'assistant', content: resp.text, toolCalls: resp.toolCalls || [] })
        sess.updatedAt = Date.now()
        persist()
      })
      .catch(() => {
        sess.messages.push({ id: uid(), role: 'assistant', content: '（回复失败，请稍后重试）' })
        persist()
      })
  }

  return {
    bots, activeBotId, activeSessionId,
    activeBot, sessions, activeSession,
    selectBot, selectSession,
    createBot, updateBot, deleteBot,
    createSession, deleteSession, sendMessage
  }
})
