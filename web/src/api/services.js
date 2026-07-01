// ============================================================================
// API 服务层 — 前端组件唯一依赖入口
//
// 每个函数对应 thinkbot 后端一个接口。当前 USE_MOCK=true 走 mock；
// 后端就绪后将每个函数体替换为 request(method, url, body) 即可，签名与返回结构不变。
//
// 返回值均为「解包后的 data」（Promise）。失败时 reject Error。
// ============================================================================

import { USE_MOCK, mockResolve, request } from './http'
import { db, saveDB, nowISO } from './mockDb'

let _seq = 1000
const genId = (p = 'id') => `${p}-${++_seq}`

// ============================ 1. 认证 ============================

export const authApi = {
  // POST /api/auth/login → LoginResp
  login(username, password) {
    if (USE_MOCK) {
      return mockResolve(() => {
        if (!username || !password) throw new Error('用户名或密码不能为空')
        const u = db().currentUser
        u.username = username
        u.displayName = u.displayName || username
        u.lastLoginAt = nowISO()
        saveDB()
        return { id: u.id, username: u.username, role: u.role, displayName: u.displayName, avatar: u.avatar, lastLoginAt: u.lastLoginAt }
      })
    }
    return request('POST', '/api/auth/login', { username, password })
  },
  // POST /api/auth/logout
  logout() {
    if (USE_MOCK) return mockResolve(() => null)
    return request('POST', '/api/auth/logout')
  },
  // GET /api/auth/me → LoginResp
  me() {
    if (USE_MOCK) return mockResolve(() => ({ ...db().currentUser }))
    return request('GET', '/api/auth/me')
  },
  // PUT /api/auth/password
  changePassword(oldPassword, newPassword) {
    if (USE_MOCK) {
      return mockResolve(() => {
        if (!newPassword || newPassword.length < 6) throw new Error('新密码至少 6 位')
        return null
      })
    }
    return request('PUT', '/api/auth/password', { oldPassword, newPassword })
  }
}

// ============================ 2. 用户管理 ============================

export const userApi = {
  list() {
    if (USE_MOCK) return mockResolve(() => db().users.map(u => ({ ...u })))
    return request('GET', '/api/users')
  },
  get(id) {
    if (USE_MOCK) return mockResolve(() => ({ ...db().users.find(u => u.id === id) }))
    return request('GET', `/api/users/${id}`)
  },
  create(payload) {
    // payload: {username,password,email,role,displayName}
    if (USE_MOCK) {
      return mockResolve(() => {
        const u = { id: ++_seq, username: payload.username, email: payload.email || '', role: payload.role || 'member', status: 'active', displayName: payload.displayName || payload.username, avatar: '', createdAt: nowISO(), updatedAt: nowISO() }
        db().users.push(u)
        saveDB()
        return { ...u }
      })
    }
    return request('POST', '/api/users', payload)
  },
  update(id, payload) {
    // payload: {email?,displayName?,avatar?}
    if (USE_MOCK) {
      return mockResolve(() => {
        const u = db().users.find(x => x.id === id)
        Object.assign(u, payload, { updatedAt: nowISO() })
        saveDB()
        return null
      })
    }
    return request('PUT', `/api/users/${id}`, payload)
  },
  remove(id) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const list = db().users
        const i = list.findIndex(x => x.id === id)
        if (i > -1) list.splice(i, 1)
        saveDB()
        return null
      })
    }
    return request('DELETE', `/api/users/${id}`)
  },
  setRole(id, role) {
    if (USE_MOCK) {
      return mockResolve(() => { const u = db().users.find(x => x.id === id); u.role = role; saveDB(); return null })
    }
    return request('PUT', `/api/users/${id}/role`, { role })
  },
  disable(id) {
    if (USE_MOCK) return mockResolve(() => { db().users.find(x => x.id === id).status = 'disabled'; saveDB(); return null })
    return request('PUT', `/api/users/${id}/disable`)
  },
  enable(id) {
    if (USE_MOCK) return mockResolve(() => { db().users.find(x => x.id === id).status = 'active'; saveDB(); return null })
    return request('PUT', `/api/users/${id}/enable`)
  },
  resetPassword(id, password) {
    if (USE_MOCK) return mockResolve(() => null)
    return request('PUT', `/api/users/${id}/password`, { password })
  }
}

// ============================ 3. Bot 管理 ============================

export const botApi = {
  list() {
    if (USE_MOCK) return mockResolve(() => db().bots.map(stripBot))
    return request('GET', '/api/bots')
  },
  get(id) {
    if (USE_MOCK) return mockResolve(() => stripBot(db().bots.find(b => b.id === id)))
    return request('GET', `/api/bots/${id}`)
  },
  create(payload) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const b = { avatar: '🆕', sessions: [], running: false, status: 'stopped', createdAt: nowISO(), updatedAt: nowISO(), temperature: 0.7, maxTokens: 4096, workers: 4, reasoningEffort: '', llmMain: '', llmLight: '', model: '', systemPrompt: '', ...payload }
        db().bots.push(b)
        saveDB()
        return stripBot(b)
      })
    }
    return request('POST', '/api/bots', payload)
  },
  update(id, payload) {
    if (USE_MOCK) {
      return mockResolve(() => { const b = db().bots.find(x => x.id === id); Object.assign(b, payload, { updatedAt: nowISO() }); saveDB(); return null })
    }
    return request('PUT', `/api/bots/${id}`, payload)
  },
  remove(id) {
    if (USE_MOCK) {
      return mockResolve(() => { const l = db().bots; const i = l.findIndex(x => x.id === id); if (i > -1) l.splice(i, 1); saveDB(); return null })
    }
    return request('DELETE', `/api/bots/${id}`)
  },
  start(id) {
    if (USE_MOCK) return mockResolve(() => { const b = db().bots.find(x => x.id === id); b.running = true; b.status = 'running'; saveDB(); return null })
    return request('POST', `/api/bots/${id}/start`)
  },
  stop(id) {
    if (USE_MOCK) return mockResolve(() => { const b = db().bots.find(x => x.id === id); b.running = false; b.status = 'stopped'; saveDB(); return null })
    return request('POST', `/api/bots/${id}/stop`)
  }
}

// 去掉前端本地扩展字段（avatar/sessions），仅保留后端 DTO 字段 + running
function stripBot(b) {
  if (!b) return null
  const { sessions, ...rest } = b
  return { ...rest }
}

// ============================ 4. LLM 服务商 / 模型 ============================
//
// 新契约（替代旧的扁平 /api/llm/models）：
//   GET    /api/providers                         → ProviderResp[]
//   POST   /api/providers                         → ProviderResp
//   PUT    /api/providers/:pid                    → 更新服务商基本信息/启用态
//   DELETE /api/providers/:pid
//   POST   /api/providers/:pid/test               → { ok, latencyMs?, message? }
//   POST   /api/providers/:pid/models             → ModelResp（新增单个模型）
//   PUT    /api/providers/:pid/models/:mid        → 更新模型
//   DELETE /api/providers/:pid/models/:mid
//   POST   /api/providers/:pid/models/import      → ModelResp[]（按 baseUrl 拉取可用模型）
//
// ProviderResp = { id, name, clientType, baseUrl, apiKey(脱敏), enabled, models: ModelResp[] }
// ModelResp    = { id, name, capabilities: string[], contextLength, multimodal, temperature, maxTokens }
export const providerApi = {
  list() {
    if (USE_MOCK) return mockResolve(() => db().providers.map(p => ({ ...p, models: p.models.map(m => ({ ...m })) })))
    return request('GET', '/api/providers')
  },
  create(payload) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const p = {
          id: payload.id || genId('prov'),
          name: payload.name || '新服务商',
          clientType: payload.clientType || 'OpenAI Compatible',
          baseUrl: payload.baseUrl || '',
          apiKey: maskKey(payload.apiKey),
          enabled: payload.enabled ?? false,
          models: []
        }
        db().providers.push(p)
        saveDB()
        return { ...p }
      })
    }
    return request('POST', '/api/providers', payload)
  },
  update(pid, payload) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const p = db().providers.find(x => x.id === pid)
        if (!p) throw new Error('服务商不存在')
        const next = { ...payload }
        delete next.models
        if (next.apiKey) next.apiKey = maskKey(next.apiKey)
        else delete next.apiKey
        Object.assign(p, next)
        saveDB()
        return { ...p, models: p.models.map(m => ({ ...m })) }
      })
    }
    return request('PUT', `/api/providers/${pid}`, payload)
  },
  remove(pid) {
    if (USE_MOCK) {
      return mockResolve(() => { const l = db().providers; const i = l.findIndex(x => x.id === pid); if (i > -1) l.splice(i, 1); saveDB(); return null })
    }
    return request('DELETE', `/api/providers/${pid}`)
  },
  test(pid) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const p = db().providers.find(x => x.id === pid)
        if (!p) throw new Error('服务商不存在')
        if (!p.apiKey) return { ok: false, message: '未配置 API Key' }
        return { ok: true, latencyMs: 120 + Math.round(Math.random() * 200), message: '连接成功' }
      })
    }
    return request('POST', `/api/providers/${pid}/test`)
  },
  addModel(pid, payload) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const p = db().providers.find(x => x.id === pid)
        if (!p) throw new Error('服务商不存在')
        const m = {
          id: payload.id || genId('model'),
          name: payload.name || payload.id || '新模型',
          capabilities: payload.capabilities || ['chat'],
          contextLength: payload.contextLength || 0,
          multimodal: payload.multimodal ?? false,
          temperature: payload.temperature ?? 0.7,
          maxTokens: payload.maxTokens ?? 4096
        }
        p.models.push(m)
        saveDB()
        return { ...m }
      })
    }
    return request('POST', `/api/providers/${pid}/models`, payload)
  },
  updateModel(pid, mid, payload) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const p = db().providers.find(x => x.id === pid)
        const m = p?.models.find(x => x.id === mid)
        if (!m) throw new Error('模型不存在')
        Object.assign(m, payload)
        saveDB()
        return { ...m }
      })
    }
    return request('PUT', `/api/providers/${pid}/models/${mid}`, payload)
  },
  removeModel(pid, mid) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const p = db().providers.find(x => x.id === pid)
        if (p) { const i = p.models.findIndex(x => x.id === mid); if (i > -1) p.models.splice(i, 1); saveDB() }
        return null
      })
    }
    return request('DELETE', `/api/providers/${pid}/models/${mid}`)
  },
  importModels(pid) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const p = db().providers.find(x => x.id === pid)
        if (!p) throw new Error('服务商不存在')
        // mock：模拟从远端拉取一批模型（去重后追加）
        const fetched = [
          { id: 'auto-chat-pro', name: 'Auto Chat Pro', capabilities: ['chat'], contextLength: 32000, multimodal: false, temperature: 0.7, maxTokens: 4096 },
          { id: 'auto-vision', name: 'Auto Vision', capabilities: ['chat', 'vision'], contextLength: 128000, multimodal: true, temperature: 0.7, maxTokens: 4096 }
        ]
        const exist = new Set(p.models.map(m => m.id))
        const added = fetched.filter(m => !exist.has(m.id))
        p.models.push(...added)
        saveDB()
        return added.map(m => ({ ...m }))
      })
    }
    return request('POST', `/api/providers/${pid}/models/import`)
  }
}

function maskKey(k) {
  if (!k) return ''
  if (k.length <= 12) return '*'.repeat(k.length)
  return k.slice(0, 6) + '...' + k.slice(-4)
}

// ============================ 5. Channel ============================

export const channelApi = {
  list(botId) {
    if (USE_MOCK) return mockResolve(() => db().channels.filter(c => c.botId === botId).map(c => ({ ...c })))
    return request('GET', `/api/bots/${botId}/channels`)
  },
  create(botId, payload) {
    // payload: {name,type,config}
    if (USE_MOCK) {
      return mockResolve(() => {
        const c = { id: ++_seq, botId, enabled: true, createdAt: nowISO(), updatedAt: nowISO(), ...payload }
        db().channels.push(c)
        saveDB()
        return { ...c }
      })
    }
    return request('POST', `/api/bots/${botId}/channels`, payload)
  },
  update(botId, cid, payload) {
    // payload: {name?,config?,enabled?}
    if (USE_MOCK) {
      return mockResolve(() => { const c = db().channels.find(x => x.id === cid); Object.assign(c, payload, { updatedAt: nowISO() }); saveDB(); return null })
    }
    return request('PUT', `/api/bots/${botId}/channels/${cid}`, payload)
  },
  remove(botId, cid) {
    if (USE_MOCK) {
      return mockResolve(() => { const l = db().channels; const i = l.findIndex(x => x.id === cid); if (i > -1) l.splice(i, 1); saveDB(); return null })
    }
    return request('DELETE', `/api/bots/${botId}/channels/${cid}`)
  },
  // GET /api/channels/types → { types: [...] }
  types() {
    if (USE_MOCK) return mockResolve(() => CHANNEL_TYPES)
    return request('GET', '/api/channels/types')
  }
}

const CHANNEL_TYPES = {
  types: [
    {
      type: 'telegram', displayName: 'Telegram', description: 'Telegram Bot 接入', icon: 'telegram',
      fields: [
        { key: 'token', label: 'Bot Token', type: 'password', required: true, helpText: '从 @BotFather 获取' },
        { key: 'pollTimeout', label: '轮询超时(秒)', type: 'number', required: false, default: '30' },
        { key: 'apiBaseUrl', label: 'API Base URL', type: 'string', required: false },
        { key: 'parseMode', label: '解析模式', type: 'select', required: false, default: '', options: ['', 'HTML', 'MarkdownV2'] },
        { key: 'allowedUpdates', label: '允许的更新类型', type: 'string', required: false }
      ]
    },
    {
      type: 'misskey', displayName: 'Misskey', description: 'Misskey 实例接入', icon: 'misskey',
      fields: [
        { key: 'host', label: '实例域名', type: 'string', required: true, helpText: '如 misskey.io' },
        { key: 'token', label: 'Access Token', type: 'password', required: true },
        { key: 'subscribeTimeline', label: '订阅时间线', type: 'boolean', required: false, default: 'true' }
      ]
    }
  ]
}

// ============================ 6. 定时任务（cron.Job，snake_case） ============================

export const cronApi = {
  // GET /api/bots/:id/cron → { jobs, total }
  list(botId) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const jobs = db().cronJobs.filter(j => j.botId === botId).map(j => ({ ...j }))
        return { jobs, total: jobs.length }
      })
    }
    return request('GET', `/api/bots/${botId}/cron`)
  },
  get(botId, jobId) {
    if (USE_MOCK) return mockResolve(() => ({ ...db().cronJobs.find(j => j.id === jobId) }))
    return request('GET', `/api/bots/${botId}/cron/${jobId}`)
  },
  create(botId, payload) {
    // payload: {name,prompt,schedule,model?,channel?,skills?,feature?,maxRuns?,tags?}
    if (USE_MOCK) {
      return mockResolve(() => {
        const j = {
          id: genId('job'), botId,
          name: payload.name, prompt: payload.prompt,
          description: payload.description || '',
          model: payload.model || '', channel: payload.channel || '',
          skills: payload.skills || [], feature: payload.feature || 'cron',
          schedule: payload.schedule, schedule_kind: 'cron', schedule_display: payload.schedule,
          max_runs: payload.maxRuns || 0, run_count: 0,
          enabled: payload.enabled !== false, state: payload.enabled === false ? 'paused' : 'active',
          next_run_at: nowISO(86400_000), last_run_at: null, last_result: '', last_error: '',
          created_at: nowISO(), updated_at: nowISO(), tags: payload.tags || []
        }
        db().cronJobs.push(j)
        saveDB()
        return { ...j }
      })
    }
    return request('POST', `/api/bots/${botId}/cron`, payload)
  },
  update(botId, jobId, payload) {
    if (USE_MOCK) {
      return mockResolve(() => { const j = db().cronJobs.find(x => x.id === jobId); applyCronPatch(j, payload); saveDB(); return null })
    }
    return request('PUT', `/api/bots/${botId}/cron/${jobId}`, payload)
  },
  remove(botId, jobId) {
    if (USE_MOCK) {
      return mockResolve(() => { const l = db().cronJobs; const i = l.findIndex(x => x.id === jobId); if (i > -1) l.splice(i, 1); saveDB(); return null })
    }
    return request('DELETE', `/api/bots/${botId}/cron/${jobId}`)
  },
  pause(botId, jobId) {
    if (USE_MOCK) return mockResolve(() => { const j = db().cronJobs.find(x => x.id === jobId); j.enabled = false; j.state = 'paused'; saveDB(); return null })
    return request('POST', `/api/bots/${botId}/cron/${jobId}/pause`)
  },
  resume(botId, jobId) {
    if (USE_MOCK) return mockResolve(() => { const j = db().cronJobs.find(x => x.id === jobId); j.enabled = true; j.state = 'active'; saveDB(); return null })
    return request('POST', `/api/bots/${botId}/cron/${jobId}/resume`)
  },
  trigger(botId, jobId) {
    if (USE_MOCK) return mockResolve(() => { const j = db().cronJobs.find(x => x.id === jobId); j.run_count++; j.last_run_at = nowISO(); j.last_result = 'ok'; saveDB(); return null })
    return request('POST', `/api/bots/${botId}/cron/${jobId}/trigger`)
  }
}

function applyCronPatch(j, p) {
  if (p.name != null) j.name = p.name
  if (p.description != null) j.description = p.description
  if (p.prompt != null) j.prompt = p.prompt
  if (p.schedule != null) { j.schedule = p.schedule; j.schedule_display = p.schedule }
  if (p.model != null) j.model = p.model
  if (p.channel != null) j.channel = p.channel
  if (p.feature != null) j.feature = p.feature
  if (p.maxRuns != null) j.max_runs = p.maxRuns
  if (p.enabled != null) { j.enabled = p.enabled; j.state = p.enabled ? 'active' : 'paused' }
  j.updated_at = nowISO()
}

// ============================ 7. 梦境巩固 ============================

export const dreamingApi = {
  getConfig(botId) {
    if (USE_MOCK) return mockResolve(() => ({ ...db().dreaming[botId] || { enabled: false, schedule: '0 3 * * *' } }))
    return request('GET', `/api/bots/${botId}/dreaming`)
  },
  updateConfig(botId, payload) {
    // payload: {enabled?,schedule?}
    if (USE_MOCK) {
      return mockResolve(() => { db().dreaming[botId] = { ...db().dreaming[botId], ...payload }; saveDB(); return null })
    }
    return request('PUT', `/api/bots/${botId}/dreaming`, payload)
  },
  status(botId) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const cfg = db().dreaming[botId]
        if (!cfg || !cfg.enabled) return { enabled: false }
        return {
          enabled: true, running: true,
          cronJob: { id: `dream-${botId}`, name: '梦境巩固', schedule: cfg.schedule, scheduleDisplay: cfg.schedule, state: 'active', nextRunAt: nowISO(86400_000), lastRunAt: nowISO(-86400_000), lastResult: 'ok', runCount: 7 }
        }
      })
    }
    return request('GET', `/api/bots/${botId}/dreaming/status`)
  },
  trigger(botId) {
    if (USE_MOCK) {
      return mockResolve(() => ({ lightIngested: 12, lightDeduped: 3, lightDropped: 1, remThemes: 4, remCandidates: 8, deepScored: 6, deepPassed: 4, deepPromoted: 2, duration: '1.2s', phase: 'done', error: '' }))
    }
    return request('POST', `/api/bots/${botId}/dreaming/trigger`)
  }
}

// ============================ 8. 记忆 ============================

export const memoryApi = {
  query(botId, tier, limit = 20) {
    if (USE_MOCK) {
      return mockResolve(() => {
        let entries = (db().memory[botId] || []).map(m => ({ ...m }))
        if (tier) entries = entries.filter(e => e.tier === tier)
        return { entries: entries.slice(0, limit), total: entries.length, tier: tier || '' }
      })
    }
    const q = new URLSearchParams({ limit: String(limit) })
    if (tier) q.set('tier', tier)
    return request('GET', `/api/bots/${botId}/memory?${q}`)
  },
  stats(botId) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const list = db().memory[botId] || []
        return { l1Count: list.filter(m => m.tier === 'L1').length, l2Estimate: list.filter(m => m.tier === 'L2').length }
      })
    }
    return request('GET', `/api/bots/${botId}/memory/stats`)
  }
}

// ============================ 9. 技能 ============================

export const skillApi = {
  list() {
    if (USE_MOCK) return mockResolve(() => { const skills = db().skills.map(s => ({ ...s })); return { skills, total: skills.length } })
    return request('GET', '/api/skills')
  },
  get(name) {
    if (USE_MOCK) return mockResolve(() => ({ ...db().skills.find(s => s.name === name) }))
    return request('GET', `/api/skills/${name}`)
  },
  enable(name) {
    if (USE_MOCK) return mockResolve(() => { db().skills.find(s => s.name === name).enabled = true; saveDB(); return { name, enabled: true } })
    return request('PUT', `/api/skills/${name}/enable`)
  },
  disable(name) {
    if (USE_MOCK) return mockResolve(() => { db().skills.find(s => s.name === name).enabled = false; saveDB(); return { name, enabled: false } })
    return request('PUT', `/api/skills/${name}/disable`)
  }
}

// ============================ 10. 统计 ============================
//
// 后端现有：GET /api/stats/overview、/api/stats/bots/:id、/api/stats/bots/:id/daily（按日聚合）
// 新增契约（图片 Usage 页所需，后端待补）：
//   GET /api/stats/daily?from&to[&botId]      → DailyStat[]（含 cache token 三项，供图表）
//   GET /api/stats/records?from&to&page&pageSize[&botId]
//        → { total, page, pageSize, items: RecordItem[] }
//        RecordItem = { id, time, botId, botName, feature(Type), model, cacheReadTokens, inputTokens, outputTokens }
//        说明：provider 后端当前未落库，按 model→provider 推导；feature 即后端 feature 维度。
//   GET /api/stats/by-bot-model?from&to       → BotModelStat[]（每个 bot 在每个模型的用量）
export const statsApi = {
  overview() {
    if (USE_MOCK) {
      return mockResolve(() => db().bots.flatMap(b => statModelsOf(b).map(m => mkBotStat(b.id, m))))
    }
    return request('GET', '/api/stats/overview')
  },
  bot(id) {
    if (USE_MOCK) return mockResolve(() => statModelsOf(db().bots.find(b => b.id === id)).map(m => mkBotStat(id, m)))
    return request('GET', `/api/stats/bots/${id}`)
  },
  // 每个 Bot × 每个模型的用量明细（直观反映各 bot 在各模型上的使用情况）
  byBotModel({ from, to } = {}) {
    if (USE_MOCK) {
      return mockResolve(() => db().bots.flatMap(b =>
        statModelsOf(b).map(m => ({ ...mkBotStat(b.id, m), botName: b.name }))
      ))
    }
    return request('GET', `/api/stats/by-bot-model${qs({ from, to })}`)
  },
  daily(id, { from, to } = {}) {
    if (USE_MOCK) return mockResolve(() => mkDaily(28))
    return request('GET', `/api/stats/bots/${id}/daily${qs({ from, to })}`)
  },
  // 全局按日序列（图表用，可不指定 bot）
  dailyRange({ from, to, botId } = {}) {
    if (USE_MOCK) return mockResolve(() => mkDaily(28))
    return request('GET', `/api/stats/daily${qs({ from, to, botId })}`)
  },
  // 按日 × 各 Bot 用量（Cache Breakdown 按 bot 堆叠用）
  //   GET /api/stats/daily-by-bot?from&to
  //   → { bots: [{id,name}], series: [{ date, usage: { <botId>: tokens } }] }
  dailyByBot({ from, to } = {}) {
    if (USE_MOCK) return mockResolve(() => mkDailyByBot(28))
    return request('GET', `/api/stats/daily-by-bot${qs({ from, to })}`)
  },
  // 调用流水明细（分页）
  records({ from, to, page = 1, pageSize = 20, botId } = {}) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const all = mkRecords()
        const filtered = botId ? all.filter(r => r.botId === botId) : all
        const start = (page - 1) * pageSize
        return { total: filtered.length, page, pageSize, items: filtered.slice(start, start + pageSize) }
      })
    }
    return request('GET', `/api/stats/records${qs({ from, to, page, pageSize, botId })}`)
  }
}

function qs(obj) {
  const p = Object.entries(obj).filter(([, v]) => v !== undefined && v !== null && v !== '')
  return p.length ? '?' + p.map(([k, v]) => `${k}=${encodeURIComponent(v)}`).join('&') : ''
}

// model → provider 推导（后端暂无 provider 维度，前端按映射展示）
function providerOf(model) {
  const m = (model || '').toLowerCase()
  if (m.includes('gpt') || m.includes('o1') || m.includes('o3')) return 'OpenAI'
  if (m.includes('claude')) return 'Anthropic'
  if (m.includes('deepseek')) return 'DeepSeek'
  if (m.includes('minimax') || m.includes('grok') || m.includes('moonshot')) return 'OpenRouter'
  return 'OpenRouter'
}

// 取某个 bot 实际涉及的模型列表（主模型 + 轻量模型去重）
function statModelsOf(b) {
  if (!b) return ['gpt-4o']
  const list = [b.model, b.llmLight].filter(Boolean)
  const uniq = [...new Set(list)]
  return uniq.length ? uniq : ['gpt-4o']
}

function mkBotStat(botId, model) {
  const input = 2000 + Math.round(Math.random() * 4000)
  const output = 800 + Math.round(Math.random() * 1500)
  const hit = 30 + Math.round(Math.random() * 40)
  const miss = 30 + Math.round(Math.random() * 50)
  return {
    botId, model: model || 'gpt-4o', provider: providerOf(model),
    totalRequests: hit + miss, cacheHitRequests: hit, cacheMissRequests: miss,
    cacheReadTokens: 800 + Math.round(Math.random() * 3000),
    cacheWriteTokens: 200 + Math.round(Math.random() * 1000),
    nonCacheTokens: 1500 + Math.round(Math.random() * 4000),
    inputTokens: input, outputTokens: output, totalTokens: input + output,
    toolCalls: 10 + Math.round(Math.random() * 20)
  }
}

// 生成 n 天的按日序列（含 cache token 三项 + 命中率所需请求数）
function mkDaily(n) {
  return Array.from({ length: n }, (_, i) => {
    const read = Math.round((Math.random() * 0.5 + 0.2) * 3_000_000)
    const write = Math.round(Math.random() * 600_000)
    const noCache = Math.round((Math.random() * 0.6 + 0.3) * 3_000_000)
    const hit = Math.round(Math.random() * 80)
    const miss = Math.round(Math.random() * 60) + 5
    return {
      date: nowISO(-(n - 1 - i) * 86400_000).slice(0, 10) + 'T00:00:00Z',
      totalRequests: hit + miss,
      cacheHitRequests: hit,
      cacheMissRequests: miss,
      cacheReadTokens: read,
      cacheWriteTokens: write,
      nonCacheTokens: noCache,
      totalTokens: read + write + noCache
    }
  })
}

// 按日 × 各 bot 用量：每天每个 bot 一个 totalTokens
function mkDailyByBot(n) {
  const bots = db().bots.map(b => ({ id: b.id, name: b.name }))
  // 给每个 bot 一个基准量级，保证视觉上有差异
  const base = {}
  bots.forEach((b, i) => { base[b.id] = (i + 1) * 400_000 })
  const series = Array.from({ length: n }, (_, i) => {
    const usage = {}
    bots.forEach(b => {
      usage[b.id] = Math.round(base[b.id] * (0.4 + Math.random() * 1.2))
    })
    return { date: nowISO(-(n - 1 - i) * 86400_000).slice(0, 10) + 'T00:00:00Z', usage }
  })
  return { bots, series }
}

// 生成调用流水（明细表用），跨多个 bot/model/feature/provider
function mkRecords() {
  const bots = db().bots
  const features = ['Chat', 'Heartbeat', 'Cron', 'Dreaming', 'Tool']
  const out = []
  let baseTs = Date.now()
  // 固定生成 120 条，模拟「1-20 / N」分页
  for (let i = 0; i < 120; i++) {
    const b = bots[i % bots.length]
    const model = statModelsOf(b)[i % statModelsOf(b).length]
    baseTs -= Math.round(Math.random() * 1800_000)
    out.push({
      id: 'rec-' + i,
      time: new Date(baseTs).toISOString().replace(/\.\d{3}Z$/, 'Z'),
      botId: b.id,
      botName: b.name,
      feature: features[i % features.length],
      model,
      provider: providerOf(model),
      cacheReadTokens: Math.round(Math.random() * 40000),
      inputTokens: 1000 + Math.round(Math.random() * 49000),
      outputTokens: 50 + Math.round(Math.random() * 900)
    })
  }
  return out
}

// ============================ 11. 工作流（只读 + 恢复） ============================

export const workflowApi = {
  list() {
    if (USE_MOCK) return mockResolve(() => { const workflows = db().workflows.map(w => ({ ...w })); return { workflows, total: workflows.length } })
    return request('GET', '/api/workflows')
  },
  metrics() {
    if (USE_MOCK) return mockResolve(() => ({ submitted: 10, completed: 7, failed: 1, terminated: 0, running: 2, nodeExecuted: 50, nodeFailed: 3, nodeRetries: 5, nodeReviews: 8, nodeSkipped: 2, persistErrors: 0 }))
    return request('GET', '/api/workflows/metrics')
  },
  status(wfId) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const w = db().workflows.find(x => x.id === wfId)
        return { id: w.id, status: w.status, requirement: w.requirement, nodeCount: w.nodeCount, progress: { pending: 1, running: 1, reviewing: 0, completed: 2, failed: 1, skipped: 0 }, createdAt: w.createdAt.replace('T', ' ').replace('Z', '').slice(0, 19), error: '' }
      })
    }
    return request('GET', `/api/workflows/${wfId}`)
  },
  nodes(wfId, format = 'flat') {
    if (USE_MOCK) {
      return mockResolve(() => ({
        workflowId: wfId, status: 'running', format,
        flat: [
          { id: 'n1', name: '需求分析', status: 'completed', task: '分析需求', result: '已完成分析', error: '', dependencies: [], review: false, retryCount: 0, iterationCount: 1, startedAt: nowISO(-3600_000), completedAt: nowISO(-3500_000) },
          { id: 'n2', name: '方案设计', status: 'running', task: '设计方案', result: '', error: '', dependencies: ['n1'], review: true, retryCount: 0, iterationCount: 1, startedAt: nowISO(-3000_000) }
        ]
      }))
    }
    return request('GET', `/api/workflows/${wfId}/nodes?format=${format}`)
  },
  recover() {
    if (USE_MOCK) return mockResolve(() => ({ total: 1, resumed: 1, reanalyzed: 0, failed: 0, workflowIds: ['wf-1'] }))
    return request('POST', '/api/workflows/recover')
  },
  // POST /api/workflows/:id/nodes/:nodeId/retry → { workflowId, nodeId, status }
  retryNode(wfId, nodeId) {
    if (USE_MOCK) return mockResolve(() => ({ workflowId: wfId, nodeId, status: 'running' }))
    return request('POST', `/api/workflows/${wfId}/nodes/${nodeId}/retry`)
  }
}

// ============================ 12. 系统监控 ============================

export const systemApi = {
  health() {
    if (USE_MOCK) {
      return mockResolve(() => ({ status: 'ok', host: 'thinkbot-prod', uptime: '3h2m1s', uptimeSec: 10921, goroutines: 42, memory: { allocMB: 25, totalAllocMB: 100, sysMB: 80, gcCount: 12 }, goVersion: 'go1.22', bots: { running: db().bots.filter(b => b.running).length } }))
    }
    return request('GET', '/api/system/health')
  },
  eventMetrics() {
    if (USE_MOCK) return mockResolve(() => ({ enabled: true, activeSubscriptions: 5, latestSeq: 12345, metrics: { published: 1200, dropped: 0 } }))
    return request('GET', '/api/system/events/metrics')
  }
}

// ============================ 13. 系统配置 ============================

export const configApi = {
  list() {
    if (USE_MOCK) return mockResolve(() => db().config.map(c => ({ ...c })))
    return request('GET', '/api/config')
  },
  get(key) {
    if (USE_MOCK) return mockResolve(() => { const c = db().config.find(x => x.key === key); return { key, value: c?.value || '' } })
    return request('GET', `/api/config/${key}`)
  },
  set(key, value) {
    if (USE_MOCK) return mockResolve(() => { const c = db().config.find(x => x.key === key); if (c) c.value = value; saveDB(); return null })
    return request('PUT', `/api/config/${key}`, { value })
  },
  batchSet(items) {
    // items: { key: value }
    if (USE_MOCK) {
      return mockResolve(() => { Object.entries(items).forEach(([k, v]) => { const c = db().config.find(x => x.key === k); if (c) c.value = v }); saveDB(); return null })
    }
    return request('PUT', '/api/config', { items })
  }
}

// ============================ 14. 身份绑定 ============================

export const bindApi = {
  generate() {
    if (USE_MOCK) {
      return mockResolve(() => {
        const code = 'TB-' + Math.random().toString(36).slice(2, 6).toUpperCase() + '-' + Math.random().toString(36).slice(2, 6).toUpperCase()
        const entry = { code, expiresAt: nowISO(5 * 60_000), ttlSec: 300 }
        db().bindCodes.push(entry)
        saveDB()
        return { code, expiresAt: entry.expiresAt, ttl: 5, hint: `请在 Telegram/Misskey 发送 ${code} 完成绑定，5 分钟内有效。` }
      })
    }
    return request('POST', '/api/bindcode')
  },
  listCodes() {
    if (USE_MOCK) return mockResolve(() => ({ codes: db().bindCodes.map(c => ({ ...c })) }))
    return request('GET', '/api/bindcode')
  },
  listBindings() {
    if (USE_MOCK) return mockResolve(() => ({ bindings: db().bindings.map(b => ({ ...b })) }))
    return request('GET', '/api/bindings')
  },
  removeBinding(id) {
    if (USE_MOCK) {
      return mockResolve(() => { const l = db().bindings; const i = l.findIndex(x => x.id === id); if (i > -1) l.splice(i, 1); saveDB(); return null })
    }
    return request('DELETE', `/api/bindings/${id}`)
  }
}

// ============================ 聊天（SSE 简化为整段返回） ============================

export const chatApi = {
  bots() {
    if (USE_MOCK) return mockResolve(() => db().bots.filter(b => b.running).map(b => ({ id: b.id, name: b.name, running: true })))
    return request('GET', '/api/chat/bots')
  },
  // mock 下不做真正 SSE，返回完整文本；真实接入时改为 EventSource/fetch-stream
  //
  // 返回结构（后端对齐）：
  // {
  //   traceId: string,
  //   text: string,                       // 回复正文
  //   toolCalls?: ToolCall[]              // 本轮 bot 调用的工具/动作，可为空
  // }
  // ToolCall = {
  //   id: string,
  //   name: string,                       // 工具名，如 "edit_file" / "run_command"
  //   title?: string,                     // 展示标题，缺省用 name
  //   status: 'success'|'error'|'running',
  //   runningText?: string,               // 执行中文案，如 "写入文件中"
  //   summary?: string,                   // 摘要，如 "2 个文件已更改"
  //   added?: number, removed?: number,   // 总增删行数（文件类工具）
  //   reversible?: boolean,               // 是否可撤销（预留，前端先占位）
  //   files?: Array<{ path, added, removed, status }>,  // 文件改动明细
  //   command?: string, output?: string   // 命令类工具（可选）
  // }
  // 说明：mock 下工具初始为 running，由前端 ToolCallCard 模拟推进到 _finalStatus；
  //       真实接入时由 SSE 增量推送 status 变化，无需 _finalStatus。
  send(botId, text) {
    if (USE_MOCK) {
      const bot = db().bots.find(b => b.id === botId)
      return mockResolve(() => {
        const resp = {
          traceId: genId('web'),
          text: `收到你的消息：「${text}」。我是「${bot?.name || 'Bot'}」，这是一条模拟回复。`
        }
        // 演示：当消息涉及代码/修改类意图时，附带一组工具调用卡片（初始执行中）
        if (/改|修改|代码|文件|重构|实现|删除|新增|fix|bug/i.test(text)) {
          resp.toolCalls = [
            {
              id: genId('tool'),
              name: 'edit_file',
              title: '编辑文件',
              status: 'running',
              runningText: '写入文件中',
              _finalStatus: 'success',
              summary: '2 个文件已更改',
              added: 0,
              removed: 7,
              reversible: true,
              files: [
                { path: 'src/components/BotSidebar.vue', added: 0, removed: 1, status: 'modified' },
                { path: 'src/router/index.js', added: 0, removed: 6, status: 'modified' }
              ]
            },
            {
              id: genId('tool'),
              name: 'run_command',
              title: '执行命令',
              status: 'running',
              runningText: '命令执行中',
              _finalStatus: 'success',
              summary: 'vite build',
              reversible: false,
              command: 'npm run build',
              output: '✓ built in 5.33s'
            }
          ]
        }
        return resp
      })
    }
    return request('POST', '/api/chat/send', { botId, text })
  }
}

// ============================ 9. 会话工具栏（Terminal / 文件 / 状态） ============================
// 主聊天右侧工具栏数据。后端就绪后按下列契约实现：
//   GET  /api/sessions/:sid/terminal           → { host, connected, tabs:[{id,name,active}] }
//   POST /api/sessions/:sid/terminal/exec       { cmd } → { output, cwd }
//   GET  /api/sessions/:sid/files?path=/data   → { path, entries:[{name,type:'dir'|'file',size,mtime}] }
//   POST /api/sessions/:sid/files/mkdir         { path, name } → { ok:true }
//   POST /api/sessions/:sid/files/upload        { path, name, size } → { ok:true }
//   GET  /api/sessions/:sid/status             → { messages, contextUsed, contextLimit, cacheHitRate, cacheRead, cacheWrite, skills:[] }
//   POST /api/sessions/:sid/compact            → { ok:true }

// mock 用的虚拟文件树（按目录路径索引），仅 USE_MOCK 生效
const _mockFS = {
  '/data': [
    { name: '.memoh', type: 'dir', size: 0, mtime: '2026-04-25T00:00:00Z' },
    { name: 'browser-screenshots', type: 'dir', size: 0, mtime: '2026-05-05T00:00:00Z' },
    { name: 'generated-images', type: 'dir', size: 0, mtime: '2026-04-26T00:00:00Z' },
    { name: 'media', type: 'dir', size: 0, mtime: '2026-06-14T00:00:00Z' },
    { name: 'memory', type: 'dir', size: 0, mtime: '2026-06-14T00:00:00Z' },
    { name: 'skills', type: 'dir', size: 0, mtime: '2026-05-05T00:00:00Z' },
    { name: '.wget-hsts', type: 'file', size: 282, mtime: '2026-05-05T00:00:00Z' },
    { name: 'HEARTBEAT.md', type: 'file', size: 223, mtime: '2026-05-05T00:00:00Z' },
    { name: 'IDENTITY.md', type: 'file', size: 1331, mtime: '2026-04-27T00:00:00Z' },
    { name: 'MEMORY.md', type: 'file', size: 121958, mtime: '2026-05-27T00:00:00Z' },
    { name: 'PROFILES.md', type: 'file', size: 421, mtime: '2026-04-26T00:00:00Z' },
    { name: 'SOUL.md', type: 'file', size: 2252, mtime: '2026-04-27T00:00:00Z' },
    { name: 'TOOLS.md', type: 'file', size: 1024, mtime: '2026-04-25T00:00:00Z' }
  ],
  '/data/skills': [
    { name: 'pdf', type: 'dir', size: 0, mtime: '2026-05-05T00:00:00Z' },
    { name: 'docx', type: 'dir', size: 0, mtime: '2026-05-05T00:00:00Z' },
    { name: 'README.md', type: 'file', size: 642, mtime: '2026-05-05T00:00:00Z' }
  ],
  '/data/memory': [
    { name: 'topic', type: 'dir', size: 0, mtime: '2026-06-14T00:00:00Z' },
    { name: 'INSTRUCTIONS.md', type: 'file', size: 880, mtime: '2026-06-14T00:00:00Z' }
  ],
  '/data/generated-images': [
    { name: 'tiger.png', type: 'file', size: 204800, mtime: '2026-04-26T00:00:00Z' },
    { name: 'chart.png', type: 'file', size: 88400, mtime: '2026-04-26T00:00:00Z' }
  ]
}

function _mockExec(cmd) {
  const c = (cmd || '').trim()
  if (!c) return ''
  if (c === 'clear') return '__CLEAR__'
  if (c === 'pwd') return '/root'
  if (c === 'whoami') return 'root'
  if (c === 'ls') return '.memoh  browser-screenshots  generated-images  media  memory  skills  MEMORY.md  TOOLS.md'
  if (c === 'date') return new Date().toString()
  if (c.startsWith('echo ')) return c.slice(5)
  return `zsh: command not found: ${c.split(' ')[0]}`
}

export const sessionToolApi = {
  terminal(sid) {
    if (USE_MOCK) {
      return mockResolve(() => ({
        host: 'root@89ad94f33f6a',
        connected: true,
        tabs: [{ id: 't1', name: '终端 1', active: true }]
      }))
    }
    return request('GET', `/api/sessions/${sid}/terminal`)
  },
  exec(sid, cmd) {
    if (USE_MOCK) return mockResolve(() => ({ output: _mockExec(cmd), cwd: '~' }))
    return request('POST', `/api/sessions/${sid}/terminal/exec`, { cmd })
  },
  // path 形如 '/data'；返回该目录下条目
  files(sid, path = '/data') {
    if (USE_MOCK) {
      return mockResolve(() => ({ path, entries: (_mockFS[path] || []).slice() }))
    }
    return request('GET', `/api/sessions/${sid}/files`, undefined, { path })
  },
  mkdir(sid, path, name) {
    if (USE_MOCK) {
      return mockResolve(() => {
        if (!_mockFS[path]) _mockFS[path] = []
        if (_mockFS[path].some(e => e.name === name)) throw new Error('同名目录已存在')
        _mockFS[path].push({ name, type: 'dir', size: 0, mtime: nowISO() })
        _mockFS[path].sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'dir' ? -1 : 1))
        return { ok: true }
      })
    }
    return request('POST', `/api/sessions/${sid}/files/mkdir`, { path, name })
  },
  upload(sid, path, name, size) {
    if (USE_MOCK) {
      return mockResolve(() => {
        if (!_mockFS[path]) _mockFS[path] = []
        const idx = _mockFS[path].findIndex(e => e.name === name)
        const entry = { name, type: 'file', size: size || 0, mtime: nowISO() }
        if (idx > -1) _mockFS[path][idx] = entry
        else _mockFS[path].push(entry)
        return { ok: true }
      })
    }
    return request('POST', `/api/sessions/${sid}/files/upload`, { path, name, size })
  },
  status(sid) {
    if (USE_MOCK) {
      return mockResolve(() => ({
        messages: 2967,
        contextUsed: 79770,
        contextLimit: null,
        cacheHitRate: 0.864,
        cacheRead: 32400000,
        cacheWrite: 0,
        skills: []
      }))
    }
    return request('GET', `/api/sessions/${sid}/status`)
  },
  compact(sid) {
    if (USE_MOCK) return mockResolve(() => ({ ok: true }))
    return request('POST', `/api/sessions/${sid}/compact`)
  }
}

// ============================ Bot 详情面板（平台/记忆/访问控制/文件/聊天节奏） ============================
//
// 以下接口均按 botId 归属，mock 用内存对象惰性初始化（首访填默认数据）。
// 后端契约：
//   平台      GET/POST   /api/bots/:id/platforms              POST/PUT/DELETE /api/bots/:id/platforms[/:pid]
//   工具清单  GET        /api/bots/:id/platforms/tool-catalog
//   记忆      GET        /api/bots/:id/memory                 POST/PUT/DELETE /api/bots/:id/memory[/:mid]
//   访问控制  GET/PUT    /api/bots/:id/access                 （default + rules[]）
//   文件      GET        /api/bots/:id/files?path=            POST /files/mkdir  POST /files/upload
//   聊天节奏  GET/PUT    /api/bots/:id/chat-rhythm
// ============================================================================

// 平台可分配的工具目录（图1 工具权限矩阵）
const BOT_TOOL_CATALOG = [
  { group: 'Messaging', tools: ['send', 'reply', 'react', 'get_contacts', 'speak'] },
  { group: 'Memory', tools: ['search_memory'] },
  { group: 'Web', tools: ['web_search', 'web_fetch'] },
  { group: 'Schedule', tools: ['list_schedule', 'get_schedule', 'create_schedule', 'update_schedule', 'delete_schedule'] },
  { group: 'Container', tools: ['read', 'write', 'list', 'edit', 'exec', 'bg_status'] },
  { group: 'Email', tools: ['list_mail', 'read_mail', 'send_mail'] }
]

const PLATFORM_TYPES = [
  { type: 'dingtalk', name: '钉钉', icon: '📨', color: '#3b8fff', fields: [
    { key: 'clientId', label: 'Client ID', type: 'text', placeholder: '' },
    { key: 'clientSecret', label: 'Client Secret', type: 'password', placeholder: '' }
  ] },
  { type: 'discord', name: 'Discord', icon: '🎮', color: '#5865f2', fields: [
    { key: 'token', label: 'Bot Token', type: 'password', placeholder: '' }
  ] },
  { type: 'feishu', name: '飞书', icon: '🪶', color: '#3370ff', fields: [
    { key: 'appId', label: 'App ID', type: 'text', placeholder: '' },
    { key: 'appSecret', label: 'App Secret', type: 'password', placeholder: '' }
  ] },
  { type: 'matrix', name: 'Matrix', icon: '🌐', color: '#0dbd8b', fields: [
    { key: 'homeserver', label: 'Homeserver', type: 'text', placeholder: 'https://matrix.org' },
    { key: 'token', label: 'Access Token', type: 'password', placeholder: '' }
  ] },
  { type: 'qq', name: 'QQ', icon: '🐧', color: '#12b7f5', fields: [
    { key: 'appId', label: 'App ID', type: 'text', placeholder: '' },
    { key: 'clientSecret', label: 'Client Secret', type: 'password', placeholder: '' },
    { key: 'inputHint', label: 'Input Hint', type: 'switch', optional: true, help: 'Send QQ input-notify hints for direct messages while the bot is processing.' },
    { key: 'markdown', label: 'Markdown Support', type: 'switch', optional: true, help: 'Enable QQ markdown message mode for C2C and group replies when the bot has permission.' }
  ] },
  { type: 'slack', name: 'Slack', icon: '💬', color: '#611f69', fields: [
    { key: 'botToken', label: 'Bot Token', type: 'password', placeholder: 'xoxb-...' }
  ] },
  { type: 'telegram', name: 'Telegram', icon: '✈️', color: '#2aabee', fields: [
    { key: 'token', label: 'Bot Token', type: 'password', placeholder: '从 @BotFather 获取' }
  ] },
  { type: 'wechat_mp', name: '微信服务号', icon: '🟢', color: '#2dc100', fields: [
    { key: 'appId', label: 'App ID', type: 'text', placeholder: '' },
    { key: 'appSecret', label: 'App Secret', type: 'password', placeholder: '' }
  ] },
  { type: 'wecom', name: '企业微信', icon: '🏢', color: '#2f90ea', fields: [
    { key: 'corpId', label: 'Corp ID', type: 'text', placeholder: '' },
    { key: 'corpSecret', label: 'Corp Secret', type: 'password', placeholder: '' }
  ] },
  { type: 'wechat', name: '微信', icon: '💚', color: '#07c160', fields: [
    { key: 'token', label: 'Token', type: 'password', placeholder: '' }
  ] },
  { type: 'misskey', name: 'Misskey', icon: '🟩', color: '#86b300', fields: [
    { key: 'token', label: 'Access Token', type: 'password', placeholder: '' },
    { key: 'instanceUrl', label: 'Instance URL', type: 'text', help: 'Misskey instance URL (e.g. https://misskey.io)', placeholder: 'https://misskey.io' }
  ] }
]

const _botPlatforms = {}
function ensurePlatforms(botId) {
  if (!_botPlatforms[botId]) {
    _botPlatforms[botId] = [
      { id: genId('plat'), type: 'misskey', name: 'Misskey', enabled: true, configured: true,
        config: { token: 'mk-xxxxxxxxxxxxxxxxxxxxxxxxxxxx', instanceUrl: 'https://maid.lat' },
        tools: ['send', 'reply', 'react', 'get_contacts', 'search_memory', 'web_search'] }
    ]
  }
  return _botPlatforms[botId]
}

export const botPlatformApi = {
  toolCatalog() {
    if (USE_MOCK) return mockResolve(() => ({ catalog: BOT_TOOL_CATALOG.map(g => ({ ...g, tools: [...g.tools] })), types: PLATFORM_TYPES }))
    return request('GET', '/api/bots/platforms/tool-catalog')
  },
  list(botId) {
    if (USE_MOCK) return mockResolve(() => ensurePlatforms(botId).map(p => ({ ...p, config: { ...p.config }, tools: [...p.tools] })))
    return request('GET', `/api/bots/${botId}/platforms`)
  },
  create(botId, payload) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const meta = PLATFORM_TYPES.find(t => t.type === payload.type) || PLATFORM_TYPES[0]
        const p = { id: genId('plat'), type: meta.type, name: payload.name || meta.name, enabled: false, configured: false, config: payload.config || {}, tools: payload.tools || [] }
        ensurePlatforms(botId).push(p)
        return { ...p }
      })
    }
    return request('POST', `/api/bots/${botId}/platforms`, payload)
  },
  update(botId, pid, payload) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const p = ensurePlatforms(botId).find(x => x.id === pid)
        if (p) Object.assign(p, payload, { configured: true })
        return { ...p }
      })
    }
    return request('PUT', `/api/bots/${botId}/platforms/${pid}`, payload)
  },
  remove(botId, pid) {
    if (USE_MOCK) {
      return mockResolve(() => { const l = ensurePlatforms(botId); const i = l.findIndex(x => x.id === pid); if (i > -1) l.splice(i, 1); return null })
    }
    return request('DELETE', `/api/bots/${botId}/platforms/${pid}`)
  }
}

// ---- 记忆（图2：文件式记忆条目，左列表 + 右编辑） ----
const _botMem = {}
function ensureMem(botId) {
  if (!_botMem[botId]) {
    const base = Date.now()
    _botMem[botId] = Array.from({ length: 8 }).map((_, i) => {
      const t = new Date(base - i * 5 * 3600_000)
      const ts = `${t.getFullYear()}-${String(t.getMonth() + 1).padStart(2, '0')}-${String(t.getDate()).padStart(2, '0')} ${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}:${String(t.getSeconds()).padStart(2, '0')}`
      return { id: `mem_${t.getFullYear()}${String(t.getMonth() + 1).padStart(2, '0')}${String(t.getDate()).padStart(2, '0')}_${String(i + 1).padStart(3, '0')}`,
        title: ts,
        content: `${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')} BJT heartbeat。上次心跳以来的半个小时里，无新消息、无@提及、无新对话、无活跃聊天会话。所有系统文件一片死寂。HEARTBEAT_OK。`,
        updatedAt: ts }
    })
  }
  return _botMem[botId]
}

export const botMemoryApi = {
  list(botId) {
    if (USE_MOCK) return mockResolve(() => ensureMem(botId).map(m => ({ ...m })))
    return request('GET', `/api/bots/${botId}/memory`)
  },
  create(botId, payload) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const now = new Date()
        const ts = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')} ${String(now.getHours()).padStart(2, '0')}:${String(now.getMinutes()).padStart(2, '0')}:${String(now.getSeconds()).padStart(2, '0')}`
        const m = { id: genId('mem'), title: payload.title || ts, content: payload.content || '', updatedAt: ts }
        ensureMem(botId).unshift(m)
        return { ...m }
      })
    }
    return request('POST', `/api/bots/${botId}/memory`, payload)
  },
  update(botId, mid, payload) {
    if (USE_MOCK) {
      return mockResolve(() => { const m = ensureMem(botId).find(x => x.id === mid); if (m) Object.assign(m, payload); return { ...m } })
    }
    return request('PUT', `/api/bots/${botId}/memory/${mid}`, payload)
  },
  remove(botId, mid) {
    if (USE_MOCK) {
      return mockResolve(() => { const l = ensureMem(botId); const i = l.findIndex(x => x.id === mid); if (i > -1) l.splice(i, 1); return null })
    }
    return request('DELETE', `/api/bots/${botId}/memory/${mid}`)
  }
}

// ---- 访问控制（图3：默认行为 + 规则列表） ----
const _botAccess = {}
function ensureAccess(botId) {
  if (!_botAccess[botId]) _botAccess[botId] = { default: 'allow', rules: [] }
  return _botAccess[botId]
}
export const botAccessApi = {
  get(botId) {
    if (USE_MOCK) return mockResolve(() => { const a = ensureAccess(botId); return { default: a.default, rules: a.rules.map(r => ({ ...r })) } })
    return request('GET', `/api/bots/${botId}/access`)
  },
  update(botId, payload) {
    if (USE_MOCK) {
      return mockResolve(() => { const a = ensureAccess(botId); if (payload.default) a.default = payload.default; if (payload.rules) a.rules = payload.rules.map(r => ({ ...r })); return null })
    }
    return request('PUT', `/api/bots/${botId}/access`, payload)
  }
}

// ---- 文件（图4：/data 目录文件管理器） ----
const _botFS = {}
function ensureFS(botId) {
  if (!_botFS[botId]) {
    _botFS[botId] = {
      '/': [
        { name: 'data', type: 'dir', size: 0, mtime: nowISO(-3600_000) }
      ],
      '/data': [
        { name: '.memoh', type: 'dir', size: 0, mtime: nowISO(-30 * 86400_000) },
        { name: 'media', type: 'dir', size: 0, mtime: nowISO(-16 * 86400_000) },
        { name: 'memory', type: 'dir', size: 0, mtime: nowISO(-3 * 3600_000) },
        { name: 'HEARTBEAT.md', type: 'file', size: 323, mtime: nowISO(-30 * 86400_000) },
        { name: 'IDENTITY.md', type: 'file', size: 1638, mtime: nowISO(-30 * 86400_000) },
        { name: 'MEMORY.md', type: 'file', size: 112538, mtime: nowISO(-3 * 3600_000) },
        { name: 'PROFILES.md', type: 'file', size: 1024, mtime: nowISO(-24 * 86400_000) },
        { name: 'SOUL.md', type: 'file', size: 2765, mtime: nowISO(-30 * 86400_000) },
        { name: 'TOOLS.md', type: 'file', size: 1024, mtime: nowISO(-30 * 86400_000) }
      ],
      '/data/.memoh': [],
      '/data/media': [],
      '/data/memory': [
        { name: 'topic', type: 'dir', size: 0, mtime: nowISO(-2 * 86400_000) }
      ],
      '/data/memory/topic': []
    }
  }
  return _botFS[botId]
}
export const botFileApi = {
  list(botId, path = '/') {
    if (USE_MOCK) return mockResolve(() => (ensureFS(botId)[path] || []).map(e => ({ ...e })))
    return request('GET', `/api/bots/${botId}/files`, null, { path })
  },
  mkdir(botId, path, name) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const fs = ensureFS(botId)
        if (!fs[path]) fs[path] = []
        if (fs[path].some(e => e.name === name)) throw new Error('同名目录已存在')
        fs[path].push({ name, type: 'dir', size: 0, mtime: nowISO() })
        fs[path].sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'dir' ? -1 : 1))
        const sub = path === '/' ? `/${name}` : `${path}/${name}`
        if (!fs[sub]) fs[sub] = []
        return { ok: true }
      })
    }
    return request('POST', `/api/bots/${botId}/files/mkdir`, { path, name })
  },
  upload(botId, path, name, size) {
    if (USE_MOCK) {
      return mockResolve(() => {
        const fs = ensureFS(botId)
        if (!fs[path]) fs[path] = []
        const idx = fs[path].findIndex(e => e.name === name)
        const entry = { name, type: 'file', size: size || 0, mtime: nowISO() }
        if (idx > -1) fs[path][idx] = entry; else fs[path].push(entry)
        return { ok: true }
      })
    }
    return request('POST', `/api/bots/${botId}/files/upload`, { path, name, size })
  }
}

// ---- 聊天节奏（图5） ----
const _botRhythm = {}
function ensureRhythm(botId) {
  if (!_botRhythm[botId]) {
    _botRhythm[botId] = {
      enabled: true,
      debounce: { quietWait: 2, maxWait: 15 },
      timing: { enabled: true },
      speakTendency: 0.7,
      interrupt: { enabled: true, maxConsecutive: 3, maxRounds: 6 },
      idleComp: { enabled: true, idleWindow: 60, minIdle: 5 }
    }
  }
  return _botRhythm[botId]
}
export const botRhythmApi = {
  get(botId) {
    if (USE_MOCK) return mockResolve(() => JSON.parse(JSON.stringify(ensureRhythm(botId))))
    return request('GET', `/api/bots/${botId}/chat-rhythm`)
  },
  update(botId, payload) {
    if (USE_MOCK) {
      return mockResolve(() => { _botRhythm[botId] = JSON.parse(JSON.stringify(payload)); return null })
    }
    return request('PUT', `/api/bots/${botId}/chat-rhythm`, payload)
  }
}

// ============================ 容器管理 ============================
const _botContainer = {}
function ensureContainer(botId) {
  if (!_botContainer[botId]) {
    const cid = `workspace-71170074-aadc-4759-949c-f8ccc9dbab56`
    const base = 'sha256:0da811fd3ed46c38cea69079fa395a3d715dbdbdd5c8177107c450bf6332bbfa'
    _botContainer[botId] = {
      info: {
        containerId: cid,
        containerStatus: 'running',
        taskStatus: 'running',
        namespace: 'default',
        image: 'debian:bookworm-slim',
        cdiDevice: '未附加 GPU',
        containerPath: '',
        keepData: false,
        createdAt: '2026-05-24T16:20:11',
        updatedAt: '2026-06-29T18:30:19'
      },
      snapshots: [
        { id: genId('snap'), name: cid, version: '-', source: 'image_layer', parent: base, createdAt: '2026-05-24T16:20:11' },
        { id: genId('snap'), name: base, version: '-', source: 'image_layer', parent: '-', createdAt: '2026-04-25T23:04:04' }
      ]
    }
  }
  return _botContainer[botId]
}
export const botContainerApi = {
  get(botId) {
    if (USE_MOCK) return mockResolve(() => JSON.parse(JSON.stringify(ensureContainer(botId).info)))
    return request('GET', `/api/bots/${botId}/container`)
  },
  snapshots(botId) {
    if (USE_MOCK) return mockResolve(() => JSON.parse(JSON.stringify(ensureContainer(botId).snapshots)))
    return request('GET', `/api/bots/${botId}/container/snapshots`)
  },
  start(botId) {
    if (USE_MOCK) return mockResolve(() => { const c = ensureContainer(botId); c.info.containerStatus = 'running'; c.info.taskStatus = 'running'; return JSON.parse(JSON.stringify(c.info)) })
    return request('POST', `/api/bots/${botId}/container/start`)
  },
  stop(botId) {
    if (USE_MOCK) return mockResolve(() => { const c = ensureContainer(botId); c.info.containerStatus = 'stopped'; c.info.taskStatus = 'stopped'; return JSON.parse(JSON.stringify(c.info)) })
    return request('POST', `/api/bots/${botId}/container/stop`)
  },
  createSnapshot(botId, displayName) {
    if (USE_MOCK) return mockResolve(() => {
      const c = ensureContainer(botId)
      const snap = { id: genId('snap'), name: displayName || `snapshot-${Date.now()}`, version: '-', source: 'manual', parent: c.info.containerId, createdAt: nowISO() }
      c.snapshots.unshift(snap)
      return JSON.parse(JSON.stringify(snap))
    })
    return request('POST', `/api/bots/${botId}/container/snapshots`, { displayName })
  },
  exportData(botId) {
    if (USE_MOCK) return mockResolve(() => ({ url: `mock://export/${botId}/data.tar.gz` }))
    return request('POST', `/api/bots/${botId}/container/export`)
  },
  importData(botId, payload) {
    if (USE_MOCK) return mockResolve(() => null)
    return request('POST', `/api/bots/${botId}/container/import`, payload)
  },
  restoreData(botId) {
    if (USE_MOCK) return mockResolve(() => null)
    return request('POST', `/api/bots/${botId}/container/restore`)
  },
  remove(botId, keepData) {
    if (USE_MOCK) return mockResolve(() => { const c = ensureContainer(botId); c.info.containerStatus = 'removed'; c.info.taskStatus = 'stopped'; c.info.keepData = !!keepData; return null })
    return request('DELETE', `/api/bots/${botId}/container`, { keepData })
  }
}

// ============================ 上下文压缩（图：上下文压缩） ============================
//   配置  GET/PUT   /api/bots/:id/compaction
//   记录  GET       /api/bots/:id/compaction/history
//   清空  DELETE    /api/bots/:id/compaction/history
const _botCompaction = {}
function ensureCompaction(botId) {
  if (!_botCompaction[botId]) {
    _botCompaction[botId] = {
      config: { enabled: true, threshold: 131072, ratio: 37, model: 'deepseek-v4-flash' },
      history: [
        { id: genId('cmp'), status: 'success', time: '2026/6/14 11:46:45', cost: 8.0, error: '' },
        { id: genId('cmp'), status: 'success', time: '2026/6/14 10:41:32', cost: 12.7, error: '' },
        { id: genId('cmp'), status: 'success', time: '2026/6/14 09:43:09', cost: 11.6, error: '' },
        { id: genId('cmp'), status: 'success', time: '2026/6/14 03:42:58', cost: 10.3, error: '' },
        { id: genId('cmp'), status: 'success', time: '2026/6/14 00:23:06', cost: 12.4, error: '' },
        { id: genId('cmp'), status: 'success', time: '2026/6/13 22:23:18', cost: 17.8, error: '' },
        { id: genId('cmp'), status: 'success', time: '2026/6/13 20:59:04', cost: 12.7, error: '' },
        { id: genId('cmp'), status: 'success', time: '2026/6/13 15:47:51', cost: 13.5, error: '' },
        { id: genId('cmp'), status: 'failed', time: '2026/6/13 11:02:10', cost: 4.2, error: '模型超时' }
      ]
    }
  }
  return _botCompaction[botId]
}
export const botCompactionApi = {
  getConfig(botId) {
    if (USE_MOCK) return mockResolve(() => JSON.parse(JSON.stringify(ensureCompaction(botId).config)))
    return request('GET', `/api/bots/${botId}/compaction`)
  },
  updateConfig(botId, payload) {
    if (USE_MOCK) return mockResolve(() => { ensureCompaction(botId).config = JSON.parse(JSON.stringify(payload)); return null })
    return request('PUT', `/api/bots/${botId}/compaction`, payload)
  },
  history(botId, status) {
    if (USE_MOCK) return mockResolve(() => {
      let list = ensureCompaction(botId).history
      if (status && status !== 'all') list = list.filter(x => x.status === status)
      return { records: JSON.parse(JSON.stringify(list)), total: list.length }
    })
    return request('GET', `/api/bots/${botId}/compaction/history`, { status })
  },
  clearHistory(botId) {
    if (USE_MOCK) return mockResolve(() => { ensureCompaction(botId).history = []; return null })
    return request('DELETE', `/api/bots/${botId}/compaction/history`)
  }
}

// ============================ MCP 服务器（图：MCP） ============================
//   列表  GET     /api/bots/:id/mcp
//   新增  POST    /api/bots/:id/mcp
//   更新  PUT     /api/bots/:id/mcp/:mid
//   删除  DELETE  /api/bots/:id/mcp/:mid
//   导入  POST    /api/bots/:id/mcp/import   （JSON 配置）
const _botMcp = {}
function ensureMcp(botId) {
  if (!_botMcp[botId]) _botMcp[botId] = []
  return _botMcp[botId]
}
function newMcpServer(payload = {}) {
  return {
    id: genId('mcp'),
    name: payload.name || '未命名',
    type: payload.type || 'stdio',
    command: payload.command || '',
    args: payload.args || [],
    env: payload.env || {},
    cwd: payload.cwd || '',
    url: payload.url || '',
    headers: payload.headers || {},
    enabled: payload.enabled !== false,
    status: 'draft',
    createdAt: nowISO(),
    updatedAt: nowISO()
  }
}
export const botMcpApi = {
  list(botId) {
    if (USE_MOCK) return mockResolve(() => ({ servers: JSON.parse(JSON.stringify(ensureMcp(botId))) }))
    return request('GET', `/api/bots/${botId}/mcp`)
  },
  create(botId, payload) {
    if (USE_MOCK) return mockResolve(() => {
      const s = newMcpServer(payload)
      ensureMcp(botId).push(s)
      return JSON.parse(JSON.stringify(s))
    })
    return request('POST', `/api/bots/${botId}/mcp`, payload)
  },
  update(botId, mid, payload) {
    if (USE_MOCK) return mockResolve(() => {
      const list = ensureMcp(botId)
      const s = list.find(x => x.id === mid)
      if (s) {
        Object.assign(s, payload, { updatedAt: nowISO() })
        if (payload.enabled !== undefined || payload.command !== undefined || payload.url !== undefined) {
          s.status = s.enabled ? 'running' : 'disabled'
        }
      }
      return s ? JSON.parse(JSON.stringify(s)) : null
    })
    return request('PUT', `/api/bots/${botId}/mcp/${mid}`, payload)
  },
  remove(botId, mid) {
    if (USE_MOCK) return mockResolve(() => {
      const list = ensureMcp(botId)
      const i = list.findIndex(x => x.id === mid)
      if (i > -1) list.splice(i, 1)
      return null
    })
    return request('DELETE', `/api/bots/${botId}/mcp/${mid}`)
  },
  import(botId, config) {
    // config: { mcpServers: { name: { command, args, env, url, type, ... } } }
    if (USE_MOCK) return mockResolve(() => {
      const list = ensureMcp(botId)
      const created = []
      const servers = (config && config.mcpServers) || {}
      for (const [name, cfg] of Object.entries(servers)) {
        const type = cfg.url ? (cfg.type === 'sse' ? 'sse' : 'http') : 'stdio'
        const s = newMcpServer({
          name, type,
          command: cfg.command || '', args: cfg.args || [], env: cfg.env || {},
          cwd: cfg.cwd || '', url: cfg.url || '', headers: cfg.headers || {}
        })
        list.push(s)
        created.push(s)
      }
      return { servers: JSON.parse(JSON.stringify(created)) }
    })
    return request('POST', `/api/bots/${botId}/mcp/import`, config)
  }
}

// ============================ Bot 技能（图：技能） ============================
//   列表  GET     /api/bots/:id/skills
//   详情  GET     /api/bots/:id/skills/:sid
//   新增  POST    /api/bots/:id/skills   { content }
//   更新  PUT     /api/bots/:id/skills/:sid { content }
//   删除  DELETE  /api/bots/:id/skills/:sid
const SKILL_TEMPLATE = `---
name: my-skill
description: Brief description
---

# My Skill
`
// 从 SKILL.md 的 frontmatter 中解析 name / description
function parseSkillMeta(content) {
  const meta = { name: '', description: '' }
  const m = /^---\s*\n([\s\S]*?)\n---/.exec(content || '')
  if (m) {
    for (const line of m[1].split('\n')) {
      const kv = /^\s*([A-Za-z_]+)\s*:\s*(.*)$/.exec(line)
      if (kv) {
        const key = kv[1].toLowerCase()
        if (key === 'name') meta.name = kv[2].trim()
        if (key === 'description') meta.description = kv[2].trim()
      }
    }
  }
  return meta
}
const _botSkills = {}
function ensureSkills(botId) {
  if (!_botSkills[botId]) {
    const mk = (name, description) => ({
      id: genId('skill'), name, description,
      content: `---\nname: ${name}\ndescription: ${description}\n---\n\n# ${name}\n`,
      source: 'managed', status: 'active',
      path: `/data/skills/${name}/SKILL.md`,
      createdAt: nowISO(), updatedAt: nowISO()
    })
    _botSkills[botId] = [
      mk('bangumi', 'Manage Bangumi (番组计划) anime tracking. Use this skill whenever the user mentions anime tracking, Bangumi, 番组计划...'),
      mk('pdf', 'Use this skill whenever the user wants to do anything with PDF files. This includes reading or extracting text/tables from PDF...')
    ]
  }
  return _botSkills[botId]
}
export const botSkillApi = {
  list(botId) {
    if (USE_MOCK) return mockResolve(() => ({ skills: JSON.parse(JSON.stringify(ensureSkills(botId))) }))
    return request('GET', `/api/bots/${botId}/skills`)
  },
  get(botId, sid) {
    if (USE_MOCK) return mockResolve(() => {
      const s = ensureSkills(botId).find(x => x.id === sid)
      return s ? JSON.parse(JSON.stringify(s)) : null
    })
    return request('GET', `/api/bots/${botId}/skills/${sid}`)
  },
  create(botId, content) {
    if (USE_MOCK) return mockResolve(() => {
      const meta = parseSkillMeta(content)
      const name = meta.name || `skill-${Date.now()}`
      const s = {
        id: genId('skill'), name, description: meta.description || '',
        content: content || SKILL_TEMPLATE, source: 'managed', status: 'active',
        path: `/data/skills/${name}/SKILL.md`, createdAt: nowISO(), updatedAt: nowISO()
      }
      ensureSkills(botId).push(s)
      return JSON.parse(JSON.stringify(s))
    })
    return request('POST', `/api/bots/${botId}/skills`, { content })
  },
  update(botId, sid, content) {
    if (USE_MOCK) return mockResolve(() => {
      const s = ensureSkills(botId).find(x => x.id === sid)
      if (s) {
        const meta = parseSkillMeta(content)
        s.content = content
        if (meta.name) { s.name = meta.name; s.path = `/data/skills/${meta.name}/SKILL.md` }
        if (meta.description) s.description = meta.description
        s.updatedAt = nowISO()
      }
      return s ? JSON.parse(JSON.stringify(s)) : null
    })
    return request('PUT', `/api/bots/${botId}/skills/${sid}`, { content })
  },
  remove(botId, sid) {
    if (USE_MOCK) return mockResolve(() => {
      const list = ensureSkills(botId)
      const i = list.findIndex(x => x.id === sid)
      if (i > -1) list.splice(i, 1)
      return null
    })
    return request('DELETE', `/api/bots/${botId}/skills/${sid}`)
  },
  template() { return SKILL_TEMPLATE }
}

// ============================ Bot 心跳（图：心跳） ============================
//   配置读  GET     /api/bots/:id/heartbeat
//   配置写  PUT     /api/bots/:id/heartbeat        { enabled, interval }
//   日志    GET     /api/bots/:id/heartbeat/logs   ?status=all|normal|alert
//   清空    DELETE  /api/bots/:id/heartbeat/logs
const _botHb = {}
function fmtHbTime(t) {
  return `${t.getFullYear()}/${t.getMonth() + 1}/${t.getDate()} ${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}:${String(t.getSeconds()).padStart(2, '0')}`
}
function ensureHeartbeat(botId) {
  if (!_botHb[botId]) {
    const interval = 30
    const base = Date.now()
    const samples = [
      { status: 'alert', cost: 18.6, result: '没什么需要发警报的。周三下午四点，一切照旧，直接过了。' },
      { status: 'alert', cost: 17.7, result: '（这是我自己刚输出的心跳结论，不用再处理一遍了。15:30 BJT 一切太平，无事发生。）' },
      { status: 'alert', cost: 12.6, result: '没错，就是这结果。周三下午3点，一切照旧，风平浪静。不发送警报。收工。' },
      { status: 'normal', cost: 11.2, result: '14:30 BJT，周三下午。上次心跳（14:00 BJT）以来的半个小时里，搜了一圈——无新用户消息、无@提及、无新对话、无活跃聊天会话。Misskey 时间线继续无波澜。HEARTBEAT_OK。' },
      { status: 'normal', cost: 6.8, result: '14:00 BJT，周三下午。上次心跳（13:30 BJT）以来的半个小时里，搜了一圈——无新用户消息、无@提及、无新对话、无活跃聊天会话。Misskey 时间线继续无波澜。HEARTBEAT_OK。' },
      { status: 'normal', cost: 7.4, result: '13:30 BJT，周三正午刚过。上次心跳（13:00 BJT）以来无新消息、无@提及、无新对话、无活跃会话。系统一切正常。HEARTBEAT_OK。' },
      { status: 'normal', cost: 9.1, result: '13:00 BJT，周三午间。半小时内无用户互动、无平台新事件。Misskey 时间线平静。HEARTBEAT_OK。' }
    ]
    const logs = samples.map((s, i) => ({
      id: genId('hb'),
      status: s.status,
      time: fmtHbTime(new Date(base - i * interval * 60_000)),
      cost: s.cost,
      result: s.result
    }))
    _botHb[botId] = { config: { enabled: true, interval }, logs }
  }
  return _botHb[botId]
}
export const botHeartbeatApi = {
  getConfig(botId) {
    if (USE_MOCK) return mockResolve(() => ({ ...ensureHeartbeat(botId).config }))
    return request('GET', `/api/bots/${botId}/heartbeat`)
  },
  updateConfig(botId, payload) {
    if (USE_MOCK) return mockResolve(() => {
      const hb = ensureHeartbeat(botId)
      if (payload.enabled !== undefined) hb.config.enabled = payload.enabled
      if (payload.interval !== undefined) hb.config.interval = payload.interval
      return { ...hb.config }
    })
    return request('PUT', `/api/bots/${botId}/heartbeat`, payload)
  },
  listLogs(botId, status) {
    if (USE_MOCK) return mockResolve(() => {
      let list = ensureHeartbeat(botId).logs
      if (status && status !== 'all') list = list.filter(x => x.status === status)
      return { logs: JSON.parse(JSON.stringify(list)), total: list.length }
    })
    return request('GET', `/api/bots/${botId}/heartbeat/logs`, { status })
  },
  clearLogs(botId) {
    if (USE_MOCK) return mockResolve(() => { ensureHeartbeat(botId).logs = []; return null })
    return request('DELETE', `/api/bots/${botId}/heartbeat/logs`)
  }
}

// ============================ 搜索提供方（系统设置：搜索） ============================
//   列表  GET     /api/search/providers
//   新增  POST    /api/search/providers        { name, type }
//   更新  PUT     /api/search/providers/:id
//   删除  DELETE  /api/search/providers/:id
//   启用  PUT     /api/search/providers/:id/toggle  { enabled }
// 支持的提供方类型（下拉选项，含图标字母与主题色）
export const SEARCH_PROVIDER_TYPES = [
  { type: 'brave', label: 'Brave', letter: 'B', color: '#fb542b' },
  { type: 'bing', label: 'Bing', letter: 'b', color: '#0078d4' },
  { type: 'google', label: 'Google', letter: 'G', color: '#4285f4' },
  { type: 'tavily', label: 'Tavily', letter: 'T', color: '#3aa675' },
  { type: 'sogou', label: '搜狗', letter: 'S', color: '#fa5000' },
  { type: 'serper', label: 'Serper', letter: 'S', color: '#5468ff' },
  { type: 'searxng', label: 'SearXNG', letter: 'X', color: '#3050ff' },
  { type: 'jina', label: 'Jina', letter: 'J', color: '#e0245e' },
  { type: 'exa', label: 'Exa', letter: 'E', color: '#1a73e8' },
  { type: 'bocha', label: '博查', letter: 'B', color: '#00a870' },
  { type: 'duckduckgo', label: 'DuckDuckGo', letter: 'D', color: '#de5833' },
  { type: 'yandex', label: 'Yandex', letter: 'Y', color: '#fc3f1d' }
]
function searchTypeMeta(type) {
  return SEARCH_PROVIDER_TYPES.find(t => t.type === type) || { type, label: type, letter: (type[0] || '?').toUpperCase(), color: '#888' }
}
let _searchProviders = null
function ensureSearchProviders() {
  if (!_searchProviders) {
    const mk = (type, extra = {}) => {
      const m = searchTypeMeta(type)
      return {
        id: genId('sp'), type, name: m.label, letter: m.letter, color: m.color,
        enabled: false, apiKey: '', searchType: '', timeout: 15, baseUrl: '',
        createdAt: nowISO(), updatedAt: nowISO(), ...extra
      }
    }
    _searchProviders = [
      mk('serper', { enabled: true, baseUrl: 'https://google.serper.dev/search' }),
      mk('yandex', { searchType: 'SEARCH_TYPE_RU', baseUrl: 'https://searchapi.api.cloud.yandex.net/v2/web/search' }),
      mk('duckduckgo'),
      mk('bocha'),
      mk('exa'),
      mk('jina'),
      mk('searxng', { baseUrl: 'http://localhost:8080/search' }),
      mk('sogou'),
      mk('tavily'),
      mk('google', { searchType: 'SEARCH_TYPE_WEB' }),
      mk('bing'),
      mk('brave')
    ]
  }
  return _searchProviders
}
export const searchProviderApi = {
  types() { return SEARCH_PROVIDER_TYPES.map(t => ({ ...t })) },
  list() {
    if (USE_MOCK) return mockResolve(() => ({ providers: JSON.parse(JSON.stringify(ensureSearchProviders())) }))
    return request('GET', '/api/search/providers')
  },
  create(payload) {
    if (USE_MOCK) return mockResolve(() => {
      const m = searchTypeMeta(payload.type)
      const p = {
        id: genId('sp'), type: payload.type, name: payload.name || m.label,
        letter: m.letter, color: m.color, enabled: false, apiKey: '',
        searchType: '', timeout: 15, baseUrl: '', createdAt: nowISO(), updatedAt: nowISO()
      }
      ensureSearchProviders().push(p)
      return JSON.parse(JSON.stringify(p))
    })
    return request('POST', '/api/search/providers', payload)
  },
  update(id, payload) {
    if (USE_MOCK) return mockResolve(() => {
      const p = ensureSearchProviders().find(x => x.id === id)
      if (p) Object.assign(p, payload, { updatedAt: nowISO() })
      return p ? JSON.parse(JSON.stringify(p)) : null
    })
    return request('PUT', `/api/search/providers/${id}`, payload)
  },
  toggle(id, enabled) {
    if (USE_MOCK) return mockResolve(() => {
      const p = ensureSearchProviders().find(x => x.id === id)
      if (p) { p.enabled = enabled; p.updatedAt = nowISO() }
      return p ? JSON.parse(JSON.stringify(p)) : null
    })
    return request('PUT', `/api/search/providers/${id}/toggle`, { enabled })
  },
  remove(id) {
    if (USE_MOCK) return mockResolve(() => {
      const list = ensureSearchProviders()
      const i = list.findIndex(x => x.id === id)
      if (i > -1) list.splice(i, 1)
      return null
    })
    return request('DELETE', `/api/search/providers/${id}`)
  }
}

// ============================================================================
// Bot 终端（容器 shell）
//   真实后端建议走 WebSocket：ws(s)://<host>/api/bots/:botId/terminal
//     - 前端把 xterm 的 onData 输入通过 ws.send 发出
//     - 后端 PTY 输出通过 ws.onmessage 回来后 term.write()
//   下方 mock 仅做本地命令回显，供前端联调 UI。
// ============================================================================
const _botTermState = {}
function ensureBotTerm(botId) {
  if (!_botTermState[botId]) {
    _botTermState[botId] = { cwd: '/root', host: `root@bot-${(botId || 'x').slice(0, 8)}` }
  }
  return _botTermState[botId]
}

// 逐条命令的 mock 输出；返回字符串或特殊标记 __CLEAR__
function _mockBotExec(botId, cmd) {
  const st = ensureBotTerm(botId)
  const raw = (cmd || '').trim()
  if (!raw) return ''
  const [bin, ...args] = raw.split(/\s+/)
  switch (bin) {
    case 'clear': return '__CLEAR__'
    case 'pwd': return st.cwd
    case 'whoami': return 'root'
    case 'hostname': return st.host.split('@')[1] || 'container'
    case 'date': return new Date().toString()
    case 'uname': return args.includes('-a')
      ? 'Linux container 6.1.0-thinkbot #1 SMP x86_64 GNU/Linux'
      : 'Linux'
    case 'echo': return args.join(' ')
    case 'ls': {
      const long = args.includes('-l') || args.includes('-la') || args.includes('-al')
      const names = ['data', 'skills', 'memory', 'workspace', 'IDENTITY.md', 'SOUL.md', 'MEMORY.md']
      if (!long) return names.join('  ')
      return [
        'total 28',
        'drwxr-xr-x 4 root root 4096 Jun 30 10:12 data',
        'drwxr-xr-x 3 root root 4096 Jun 28 09:01 skills',
        'drwxr-xr-x 2 root root 4096 Jun 30 14:22 memory',
        'drwxr-xr-x 2 root root 4096 Jun 30 14:22 workspace',
        '-rw-r--r-- 1 root root 1331 Apr 27 00:00 IDENTITY.md',
        '-rw-r--r-- 1 root root 2252 Apr 27 00:00 SOUL.md',
        '-rw-r--r-- 1 root root 121958 May 27 00:00 MEMORY.md'
      ].join('\n')
    }
    case 'cd': {
      const target = args[0] || '/root'
      if (target === '..') st.cwd = st.cwd.replace(/\/[^/]+$/, '') || '/'
      else if (target.startsWith('/')) st.cwd = target
      else st.cwd = (st.cwd === '/' ? '' : st.cwd) + '/' + target
      return ''
    }
    case 'cat':
      if (!args[0]) return 'cat: missing operand'
      if (args[0] === 'IDENTITY.md') return '# Identity\nname: 元宝助手\nrole: 全能 AI 助手'
      return `cat: ${args[0]}: No such file or directory`
    case 'ps': return [
      '  PID TTY          TIME CMD',
      '    1 pts/0    00:00:00 tini',
      '    7 pts/0    00:00:03 agent',
      '   42 pts/0    00:00:00 ps'
    ].join('\n')
    case 'env': return 'HOME=/root\nPATH=/usr/local/bin:/usr/bin:/bin\nAGENT_ID=' + (botId || 'bot')
    case 'help': return [
      '可用（mock）命令: ls, cd, pwd, cat, echo, date, whoami, hostname,',
      'uname, ps, env, clear, help'
    ].join('\n')
    default:
      return `bash: ${bin}: command not found`
  }
}

export const botTerminalApi = {
  // 建立会话（mock 返回提示符信息；真实实现可返回 ws 地址与 token）
  connect(botId) {
    if (USE_MOCK) return mockResolve(() => {
      const st = ensureBotTerm(botId)
      return { host: st.host, cwd: st.cwd, connected: true, banner: `Connected to container of bot ${botId}` }
    })
    return request('GET', `/api/bots/${botId}/terminal`)
  },
  exec(botId, cmd) {
    if (USE_MOCK) return mockResolve(() => {
      const output = _mockBotExec(botId, cmd)
      return { output, cwd: ensureBotTerm(botId).cwd }
    })
    return request('POST', `/api/bots/${botId}/terminal/exec`, { cmd })
  }
}
