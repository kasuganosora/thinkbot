import type {
  ApiResponse,
  UserInfo,
  BotInfo,
  BotDefinition,
  HistoryPage,
  UserRow,
  ConfigItem,
  SystemHealth,
  StatsOverview,
  BotStats,
  BotDailyStats,
  SkillInfo,
  CronJob,
  LLMModel,
  DreamingConfig,
  DreamingStatus,
  BindCode,
  Binding,
} from '@/types/api'

const BASE = '/api'

async function request<T>(
  path: string,
  options: RequestInit = {},
): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    credentials: 'include',
    headers: { 'Content-Type': 'application/json', ...options.headers },
    ...options,
  })

  if (res.status === 401) throw new AuthError('未登录或登录已过期')

  const body: ApiResponse<T> = await res.json()
  if (body.code !== 0) throw new ApiError(body.message || '请求失败', body.code)
  return body.data
}

function get<T>(path: string) {
  return request<T>(path)
}

function post<T>(path: string, body?: unknown) {
  return request<T>(path, { method: 'POST', body: body ? JSON.stringify(body) : undefined })
}

function put<T>(path: string, body?: unknown) {
  return request<T>(path, { method: 'PUT', body: body ? JSON.stringify(body) : undefined })
}

function del<T>(path: string) {
  return request<T>(path, { method: 'DELETE' })
}

export class ApiError extends Error {
  constructor(message: string, public code: number) {
    super(message)
  }
}

export class AuthError extends Error {}

// ====================================================================
// 认证
// ====================================================================

export const authApi = {
  login: (username: string, password: string) =>
    post<UserInfo>('/auth/login', { username, password }),
  logout: () => post('/auth/logout'),
  me: () => get<UserInfo>('/auth/me'),
  changePassword: (oldPassword: string, newPassword: string) =>
    put('/auth/password', { oldPassword, newPassword }),
}

// ====================================================================
// 聊天
// ====================================================================

export const chatApi = {
  bots: () => get<BotInfo[]>('/chat/bots'),
  history: (botId: string, cursor?: string, limit = 30) => {
    const params = new URLSearchParams({ botId, limit: String(limit) })
    if (cursor) params.set('cursor', cursor)
    return get<HistoryPage>(`/chat/history?${params}`)
  },
}

// ====================================================================
// Bot 管理
// ====================================================================

export const botsApi = {
  list: () => get<(BotDefinition & { running: boolean })[]>('/bots'),
  get: (id: string) => get<BotDefinition & { running: boolean }>(`/bots/${id}`),
  create: (data: Partial<BotDefinition>) => post<BotDefinition>('/bots', data),
  update: (id: string, data: Partial<BotDefinition>) => put(`/bots/${id}`, data),
  delete: (id: string) => del(`/bots/${id}`),
  start: (id: string) => post(`/bots/${id}/start`),
  stop: (id: string) => post(`/bots/${id}/stop`),

  // 梦境巩固
  getDreaming: (id: string) => get<DreamingConfig>(`/bots/${id}/dreaming`),
  updateDreaming: (id: string, data: Partial<DreamingConfig>) =>
    put<DreamingConfig>(`/bots/${id}/dreaming`, data),
  dreamingStatus: (id: string) => get<DreamingStatus>(`/bots/${id}/dreaming/status`),
  triggerDreaming: (id: string) => post(`/bots/${id}/dreaming/trigger`),

  // 定时任务
  listCronJobs: (id: string) => get<{ jobs: CronJob[]; total: number }>(`/bots/${id}/cron`),
  createCronJob: (id: string, data: Partial<CronJob>) => post<CronJob>(`/bots/${id}/cron`, data),
  updateCronJob: (id: string, jobId: string, data: Partial<CronJob>) =>
    put<CronJob>(`/bots/${id}/cron/${jobId}`, data),
  deleteCronJob: (id: string, jobId: string) => del(`/bots/${id}/cron/${jobId}`),
  pauseCronJob: (id: string, jobId: string) => post(`/bots/${id}/cron/${jobId}/pause`),
  resumeCronJob: (id: string, jobId: string) => post(`/bots/${id}/cron/${jobId}/resume`),
  triggerCronJob: (id: string, jobId: string) => post(`/bots/${id}/cron/${jobId}/trigger`),

  // 记忆
  memory: (id: string, tier?: string, limit = 50) => {
    const params = new URLSearchParams({ limit: String(limit) })
    if (tier) params.set('tier', tier)
    return get(`/bots/${id}/memory?${params}`)
  },
  memoryStats: (id: string) => get(`/bots/${id}/memory/stats`),
}

// ====================================================================
// LLM 模型管理 (admin)
// ====================================================================

export const llmApi = {
  list: () => get<LLMModel[]>('/llm/models'),
  create: (data: Partial<LLMModel>) => post<LLMModel>('/llm/models', data),
  update: (id: string, data: Partial<LLMModel>) => put(`/llm/models/${id}`, data),
  delete: (id: string) => del(`/llm/models/${id}`),
}

// ====================================================================
// 用户管理 (admin)
// ====================================================================

export const usersApi = {
  list: () => get<UserRow[]>('/users'),
  get: (id: number) => get<UserRow>(`/users/${id}`),
  create: (data: { username: string; password: string; role?: string; displayName?: string; email?: string }) =>
    post<UserRow>('/users', data),
  update: (id: number, data: Partial<UserRow>) => put(`/users/${id}`, data),
  delete: (id: number) => del(`/users/${id}`),
  updateRole: (id: number, role: string) => put(`/users/${id}/role`, { role }),
  disable: (id: number) => put(`/users/${id}/disable`),
  enable: (id: number) => put(`/users/${id}/enable`),
  resetPassword: (id: number, password: string) => put(`/users/${id}/password`, { password }),
}

// ====================================================================
// 系统配置 (admin)
// ====================================================================

export const configApi = {
  list: () => get<ConfigItem[]>('/config'),
  get: (key: string) => get<ConfigItem>(`/config/${key}`),
  set: (key: string, value: string) => put(`/config/${key}`, { value }),
  batchSet: (items: Record<string, string>) => put('/config', { items }),
}

export const systemApi = {
  health: () => get<SystemHealth>('/system/health'),
  eventMetrics: () => get('/system/events/metrics'),
}

// ====================================================================
// 统计 (admin)
// ====================================================================

export const statsApi = {
  overview: (from?: string, to?: string) => {
    const params = new URLSearchParams()
    if (from) params.set('from', from)
    if (to) params.set('to', to)
    return get<StatsOverview>(`/stats/overview?${params}`)
  },
  bot: (botId: string, from?: string, to?: string) => {
    const params = new URLSearchParams()
    if (from) params.set('from', from)
    if (to) params.set('to', to)
    return get<BotStats>(`/stats/bots/${botId}?${params}`)
  },
  botDaily: (botId: string, from?: string, to?: string) => {
    const params = new URLSearchParams()
    if (from) params.set('from', from)
    if (to) params.set('to', to)
    return get<BotDailyStats>(`/stats/bots/${botId}/daily?${params}`)
  },
}

// ====================================================================
// 技能 (bot.manage)
// ====================================================================

export const skillsApi = {
  list: () => get<{ skills: SkillInfo[]; total: number }>('/skills'),
  get: (name: string) => get<SkillInfo>(`/skills/${name}`),
  enable: (name: string) => put(`/skills/${name}/enable`),
  disable: (name: string) => put(`/skills/${name}/disable`),
}

// ====================================================================
// 授权码与绑定
// ====================================================================

export const bindApi = {
  generate: () => post<BindCode>('/bindcode'),
  list: () => get<{ codes: BindCode[] }>('/bindcode'),
  bindings: () => get<{ bindings: Binding[] }>('/bindings'),
  deleteBinding: (id: string) => del(`/bindings/${id}`),
}
