// ============================================================================
// Mock 数据库 — 内存数据 + localStorage 持久化
//
// ⚠️ 本文件的字段名 = 后端对接契约。字段名严格对齐 thinkbot 后端 DTO。
// 注意命名风格差异：
//   - 绝大多数接口为 camelCase
//   - cron.Job 全部为 snake_case（schedule_kind / run_count / next_run_at ...）
//   - 时间统一用 RFC3339 字符串（workflow StatusResult.createdAt 例外，用 'YYYY-MM-DD HH:mm:ss'）
// ============================================================================

const LS_KEY = 'tb_mock_db_v1'

function nowISO(offsetMs = 0) {
  return new Date(Date.now() + offsetMs).toISOString().replace(/\.\d{3}Z$/, 'Z')
}

// ---------------------------------------------------------------------------
// 种子数据
// ---------------------------------------------------------------------------
function seed() {
  return {
    // 当前登录用户（auth/me、login 返回 LoginResp）
    currentUser: {
      id: 1,
      username: 'admin',
      role: 'admin', // admin | member
      displayName: '管理员',
      avatar: '',
      lastLoginAt: nowISO(-3600_000)
    },

    // --- Bot（dao.BotDefinition + running） ---
    bots: [
      {
        id: 'assistant',
        name: '元宝助手',
        systemPrompt: '你是一个乐于助人的 AI 助手。',
        llmMain: 'gpt4o-main',
        llmLight: 'gpt4o-mini',
        model: 'gpt-4o',
        temperature: 0.7,
        maxTokens: 4096,
        reasoningEffort: 'medium', // '' | minimal | low | medium | high
        workers: 4,
        status: 'running', // stopped | running
        createdAt: nowISO(-30 * 86400_000),
        updatedAt: nowISO(-2 * 86400_000),
        running: true,
        // 前端聊天用扩展字段（非后端 DTO，仅本地会话演示）
        avatar: '🤖',
        sessions: [
          {
            id: 'sess_1',
            title: '深圳交管12123查询科目一日期',
            updatedAt: Date.now() - 3600_000,
            messages: [
              { id: 'm1', role: 'user', content: '帮我查询科目一考试日期怎么操作？' },
              { id: 'm2', role: 'assistant', content: '你可以打开「交管12123」App，进入「考试预约」→「我的预约」即可查看科目一的考试日期与考场信息。' }
            ]
          },
          {
            id: 'sess_2',
            title: 'MySQL 5.7 查看表索引方法',
            updatedAt: Date.now() - 7200_000,
            messages: [
              { id: 'm3', role: 'user', content: 'MySQL 5.7 怎么查看一张表的索引？' },
              { id: 'm4', role: 'assistant', content: '可以使用：`SHOW INDEX FROM 表名;` 或查询 `information_schema.STATISTICS` 表。' }
            ]
          }
        ]
      },
      {
        id: 'coder',
        name: '代码专家',
        systemPrompt: '你是一名资深软件工程师，回答务必准确、给出可运行代码。',
        llmMain: 'claude-main',
        llmLight: 'claude-light',
        model: 'claude-3.5-sonnet',
        temperature: 0.3,
        maxTokens: 8192,
        reasoningEffort: 'high',
        workers: 8,
        status: 'running',
        createdAt: nowISO(-20 * 86400_000),
        updatedAt: nowISO(-1 * 86400_000),
        running: true,
        avatar: '💻',
        sessions: [
          { id: 'sess_3', title: '接口与 DTO 映射整理', updatedAt: Date.now() - 5000_000, messages: [] }
        ]
      },
      {
        id: 'writer',
        name: '文案写手',
        systemPrompt: '你是一名专业文案策划。',
        llmMain: 'gpt4o-main',
        llmLight: 'gpt4o-mini',
        model: 'gpt-4o-mini',
        temperature: 0.9,
        maxTokens: 4096,
        reasoningEffort: 'low',
        workers: 2,
        status: 'stopped',
        createdAt: nowISO(-10 * 86400_000),
        updatedAt: nowISO(-5 * 86400_000),
        running: false,
        avatar: '✍️',
        sessions: []
      }
    ],

    // --- 用户管理（dao.User，无 passwordHash） ---
    users: [
      { id: 1, username: 'admin', email: 'admin@thinkbot.dev', role: 'admin', status: 'active', displayName: '管理员', avatar: '', lastLoginAt: nowISO(-3600_000), createdAt: nowISO(-90 * 86400_000), updatedAt: nowISO(-86400_000) },
      { id: 2, username: 'alice', email: 'alice@thinkbot.dev', role: 'member', status: 'active', displayName: 'Alice', avatar: '', lastLoginAt: nowISO(-2 * 86400_000), createdAt: nowISO(-60 * 86400_000), updatedAt: nowISO(-2 * 86400_000) },
      { id: 3, username: 'bob', email: 'bob@thinkbot.dev', role: 'member', status: 'disabled', displayName: 'Bob', avatar: '', createdAt: nowISO(-40 * 86400_000), updatedAt: nowISO(-10 * 86400_000) }
    ],

    // --- LLM 服务商 + 模型（新契约：Provider 一级，内嵌多个 Model；apiKey 脱敏） ---
    // ProviderResp = { id, name, clientType, baseUrl, apiKey(脱敏), enabled, models: ModelResp[] }
    // ModelResp = { id, name, capabilities: string[], contextLength, multimodal, temperature, maxTokens }
    providers: [
      {
        id: 'openai', name: 'OpenAI', clientType: 'OpenAI Compatible',
        baseUrl: 'https://api.openai.com/v1', apiKey: 'sk-abc...wxyz', enabled: true,
        models: [
          { id: 'gpt-4o', name: 'GPT-4o', capabilities: ['chat', 'vision'], contextLength: 128000, multimodal: true, temperature: 0.7, maxTokens: 4096 },
          { id: 'gpt-4o-mini', name: 'GPT-4o mini', capabilities: ['chat'], contextLength: 128000, multimodal: false, temperature: 0.7, maxTokens: 4096 }
        ]
      },
      {
        id: 'anthropic', name: 'Anthropic', clientType: 'Anthropic',
        baseUrl: 'https://api.anthropic.com', apiKey: 'sk-ant...5678', enabled: true,
        models: [
          { id: 'claude-3.5-sonnet', name: 'Claude 3.5 Sonnet', capabilities: ['chat', 'vision'], contextLength: 200000, multimodal: true, temperature: 0.5, maxTokens: 8192 }
        ]
      },
      {
        id: 'deepseek', name: 'DeepSeek', clientType: 'OpenAI Compatible',
        baseUrl: 'https://api.deepseek.com', apiKey: '', enabled: false,
        models: [
          { id: 'deepseek-chat', name: 'DeepSeek Chat', capabilities: ['chat'], contextLength: 64000, multimodal: false, temperature: 0.7, maxTokens: 4096 }
        ]
      }
    ],

    // --- Channel（dao.ChannelDefinition），按 botId 归属 ---
    channels: [
      { id: 1, botId: 'assistant', name: 'telegram-主频道', type: 'telegram', config: '{"token":"123456:ABC-token"}', enabled: true, createdAt: nowISO(-20 * 86400_000), updatedAt: nowISO(-86400_000) },
      { id: 2, botId: 'coder', name: 'misskey-dev', type: 'misskey', config: '{"host":"misskey.io","token":"mk-token"}', enabled: false, createdAt: nowISO(-15 * 86400_000), updatedAt: nowISO(-3 * 86400_000) }
    ],

    // --- 定时任务（cron.Job，snake_case） ---
    cronJobs: [
      { id: 'job-1', botId: 'assistant', name: '每日早报', prompt: '总结今天的新闻要点', model: 'gpt-4o', channel: 'telegram', skills: ['search'], feature: 'cron', schedule: '0 9 * * 1-5', schedule_kind: 'cron', schedule_display: '0 9 * * 1-5', max_runs: 0, run_count: 12, enabled: true, state: 'active', next_run_at: nowISO(86400_000), last_run_at: nowISO(-3600_000), last_result: 'ok', last_error: '', created_at: nowISO(-30 * 86400_000), updated_at: nowISO(-3600_000), tags: ['daily'] },
      { id: 'job-2', botId: 'assistant', name: '周报生成', prompt: '生成本周工作周报', model: '', channel: '', skills: [], feature: 'cron', schedule: '0 18 * * 5', schedule_kind: 'cron', schedule_display: '0 18 * * 5', max_runs: 0, run_count: 4, enabled: false, state: 'paused', next_run_at: null, last_run_at: nowISO(-3 * 86400_000), last_result: 'ok', last_error: '', created_at: nowISO(-25 * 86400_000), updated_at: nowISO(-3 * 86400_000), tags: ['weekly'] }
    ],

    // --- 梦境巩固配置，按 botId ---
    dreaming: {
      assistant: { enabled: true, schedule: '0 3 * * *' },
      coder: { enabled: false, schedule: '0 4 * * *' },
      writer: { enabled: false, schedule: '0 3 * * *' }
    },

    // --- 记忆（按 botId） ---
    memory: {
      assistant: [
        { id: 'mem-1', content: '用户偏好简洁的回答', scope: 'user:123', tier: 'L1', category: 'preference', source: 'dreaming', importance: 0.8, createdAt: nowISO(-5 * 86400_000), lastAccessed: nowISO(-3600_000) },
        { id: 'mem-2', content: '用户主要使用 Vue + TDesign 技术栈', scope: 'user:123', tier: 'L2', category: 'fact', source: 'chat', importance: 0.9, createdAt: nowISO(-10 * 86400_000), lastAccessed: nowISO(-86400_000) }
      ],
      coder: [],
      writer: []
    },

    // --- 技能（skill.SkillInfo） ---
    skills: [
      { name: 'search', description: '联网搜索能力', compatibility: ['v1'], enabled: true, source: 'filesystem', hasContent: true, hasScripts: false, hasReferences: true, hasAssets: false },
      { name: 'code-runner', description: '执行代码片段', compatibility: ['v1'], enabled: true, source: 'filesystem', hasContent: true, hasScripts: true, hasReferences: false, hasAssets: false },
      { name: 'image-gen', description: '文生图能力', compatibility: ['v1'], enabled: false, source: 'builtin', hasContent: true, hasScripts: false, hasReferences: false, hasAssets: true }
    ],

    // --- 系统配置（[]config.Setting） ---
    config: [
      { key: 'chat.context_limit', value: '20', category: 'chat', description: '聊天上下文保留条数' },
      { key: 'chat.stream', value: 'true', category: 'chat', description: '是否启用流式输出' },
      { key: 'token.monthly_quota', value: '1000000', category: 'token', description: '每月 Token 配额上限' },
      { key: 'system.host_name', value: 'thinkbot-prod', category: 'system', description: '系统主机名' },
      { key: 'workflow.max_concurrency', value: '5', category: 'workflow', description: '工作流最大并发数' }
    ],

    // --- 工作流（只读监控） ---
    workflows: [
      { id: 'wf-1', status: 'running', requirement: '生成一份市场调研报告', nodeCount: 5, createdAt: nowISO(-3600_000) },
      { id: 'wf-2', status: 'completed', requirement: '重构用户认证模块', nodeCount: 8, createdAt: nowISO(-2 * 86400_000) },
      { id: 'wf-3', status: 'failed', requirement: '批量导入历史数据', nodeCount: 3, createdAt: nowISO(-5 * 86400_000) }
    ],

    // --- 身份绑定 ---
    bindings: [
      { id: 1, platform: 'telegram', platformUserId: '987654', createdAt: nowISO(-10 * 86400_000) }
    ],
    bindCodes: []
  }
}

// ---------------------------------------------------------------------------
// 持久化加载 / 保存
// ---------------------------------------------------------------------------
let _db = null

export function db() {
  if (_db) return _db
  const raw = localStorage.getItem(LS_KEY)
  _db = raw ? JSON.parse(raw) : seed()
  return _db
}

export function saveDB() {
  localStorage.setItem(LS_KEY, JSON.stringify(_db))
}

export function resetDB() {
  _db = seed()
  saveDB()
  return _db
}

export { nowISO }
