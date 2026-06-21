// ============================================================================
// ThinkBot API 类型定义
// ============================================================================

interface ApiResponse<T = unknown> {
  code: number
  message: string
  data: T
}

// --- 认证 ---

interface UserInfo {
  id: number
  username: string
  role: string
  displayName: string
  avatar: string
  lastLoginAt?: string
}

interface ChatMessage {
  id: number
  botId: string
  userId: string
  role: 'user' | 'assistant'
  content: string
  traceId: string
  createdAt: string
}

interface HistoryPage {
  messages: ChatMessage[]
  nextCursor: string | null
}

// --- Bot ---

interface BotInfo {
  id: string
  name: string
  running: boolean
}

interface BotDefinition {
  id: string
  name: string
  systemPrompt: string
  llmMain: string
  llmLight: string
  model: string
  temperature: number
  maxTokens: number
  workers: number
  reasoningEffort?: string
  running?: boolean
}

// --- 用户管理 ---

interface UserRow {
  id: number
  username: string
  role: string
  displayName: string
  email: string
  avatar: string
  disabled: boolean
  lastLoginAt?: string
}

// --- 系统配置 ---

interface ConfigItem {
  key: string
  value: string
  category?: string
  description?: string
}

interface SystemHealth {
  status: string
  host: { hostname: string; arch: string; cpuCount: number }
  uptime: string
  goroutines: number
  memory: { allocMB: number; sysMB: number; numGC: number }
  goVersion: string
  bots: { running: number }
}

// --- 统计 ---

interface StatsOverview {
  totalRequests?: number
  totalInputTokens?: number
  totalOutputTokens?: number
  totalCost?: number
  byModel?: Record<string, { requests: number; inputTokens: number; outputTokens: number }>
  bots?: Array<{ botId: string; botName: string; requests: number; inputTokens: number; outputTokens: number }>
}

interface BotStats {
  botId: string
  totalRequests?: number
  totalInputTokens?: number
  totalOutputTokens?: number
  byModel?: Record<string, { requests: number; inputTokens: number; outputTokens: number }>
}

interface BotDailyStats {
  daily?: Array<{ date: string; requests: number; inputTokens: number; outputTokens: number }>
}

// --- 技能 ---

interface SkillInfo {
  name: string
  description: string
  enabled: boolean
  category?: string
}

// --- 定时任务 ---

interface CronJob {
  id: string
  name: string
  prompt: string
  schedule: string
  model?: string
  channel?: string
  enabled: boolean
  maxRuns?: number
  runCount?: number
  lastRunAt?: string
}

// --- LLM 模型管理 ---

interface LLMModel {
  id: string
  provider: string
  model: string
  apiKey: string
  baseUrl: string
  chatPath?: string
  temperature?: number
  maxTokens?: number
  multimodal?: boolean
}

// --- 梦境巩固 ---

interface DreamingConfig {
  enabled: boolean
  schedule: string
}

interface DreamingStatus {
  enabled: boolean
  running: boolean
  cronJob?: unknown
  schedulerSummary?: unknown
}

// --- 绑定码 ---

interface BindCode {
  code: string
  expiresAt: string
  ttl: number
  hint?: string
}

interface Binding {
  id: string
  platform: string
  platformUserId: string
  createdAt: string
}

// --- SSE ---

type SSEEventType = 'start' | 'text_delta' | 'tool_call' | 'tool_result' | 'done' | 'error'

interface SSEEvent {
  type: SSEEventType
  data: Record<string, unknown>
}

interface ToolCallInfo {
  tool: string
  input: unknown
  status: 'calling' | 'done' | 'error'
  output?: unknown
  error?: string
}

export type {
  ApiResponse,
  UserInfo,
  ChatMessage,
  HistoryPage,
  BotInfo,
  BotDefinition,
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
  SSEEventType,
  SSEEvent,
  ToolCallInfo,
}
