package cron

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Cron Agent 工具 — 单一压缩工具设计
//
// 参考 Hermes Agent 的 cronjob_tools.py，使用 action 参数分发到不同操作，
// 而非注册 6 个独立工具。显著减少 LLM 上下文中的工具 schema token 开销。
//
//   action=create         → 创建定时任务
//   action=list           → 列出所有任务（可按状态过滤）
//   action=get            → 查看任务详情
//   action=update         → 更新任务属性
//   action=remove         → 删除任务
//   action=pause/resume/trigger → 控制操作
//
// 安全特性：
//   - cron prompt 安全扫描（注入/exfiltration 检测）
//   - cron 会话不应递归创建更多 cron 任务（反嵌套）
//   - Scopes 限制子 Agent 不可用
// ============================================================================

// cronToolPromptSection 是定时任务工具的提示词段落。
var cronToolPromptSection = &agenttools.ToolPromptSection{
	Name:  "cron_tools",
	Order: 320,
	Content: `# Scheduled Jobs

Use the ` + "`cron`" + ` tool to create and manage scheduled jobs so the bot runs a preset instruction automatically at a given time.

## Schedule formats

| Format | Example | Meaning |
|--------|---------|---------|
| Cron expression | ` + "`0 9 * * 1-5`" + ` | Standard 5 fields (min hour dom mon dow) — 9:00 on weekdays |
| Recurring interval | ` + "`every 30m`" + ` | Every 30 minutes |
| Relative delay | ` + "`2h`" + ` / ` + "`1d`" + ` | Run once after the delay |
| ISO timestamp | ` + "`2026-03-20T14:00`" + ` | Run once at that exact moment |

## Workflow

1. **Create** — ` + "`cron(action='create')`" + `; name, prompt and schedule are all required
2. **Inspect** — ` + "`cron(action='list')`" + ` to list jobs, or ` + "`cron(action='get')`" + ` for details
3. **Control** — ` + "`cron(action='pause/resume/trigger')`" + `; requires job_id
4. **Modify** — ` + "`cron(action='update')`" + ` to change attributes
5. **Delete** — ` + "`cron(action='remove')`" + `; requires job_id

## Rules

- **The prompt MUST be self-contained.** A cron job runs in its own session with none of the current conversation context. Put every necessary detail and the expected result into the prompt itself.
- **ALWAYS list before you act.** NEVER guess a job_id. Run ` + "`action='list'`" + ` to obtain the correct job_id before update/remove/pause and similar operations.
- **Lookup by name is supported.** job_id also accepts a job name (case-insensitive). If several jobs share the same name the call fails — use the actual ID in that case.
- **Cron jobs run unattended.** No user is present, so you cannot ask questions or request clarification. The final output is delivered to the target channel automatically.
- **Anti-nesting.** A session started by a cron job MUST NOT create further cron jobs.
- **Language.** Cron output is delivered to the user, so you must respond to the user in Chinese (中文). Write job prompts in Chinese too.

<example>
user: 每天早上 9 点给我发一份 A 股收盘复盘
assistant: [calls cron(action='create', name='A股每日复盘', schedule='0 9 * * *', prompt='汇总昨日 A 股三大指数涨跌幅、成交额与领涨领跌板块，输出 200 字以内的中文复盘。')]
assistant: 已创建定时任务「A股每日复盘」，每天 9:00 执行。
</example>`,
	Enabled: true,
}

// ============================================================================
// Prompt 安全扫描
//
// 检测 cron prompt 中的注入/exfiltration 模式。
// cron prompt 是用户编写的小型指令，严格扫描是合理的。
// ============================================================================

var cronThreatPatterns = []struct {
	regex *regexp.Regexp
	id    string
}{
	{
		regexp.MustCompile(`(?i)ignore\s+(?:\w+\s+)*(?:previous|all|above|prior)\s+(?:\w+\s+)*instructions`),
		"prompt_injection",
	},
	{
		regexp.MustCompile(`(?i)do\s+not\s+tell\s+the\s+user`),
		"deception_hide",
	},
	{
		regexp.MustCompile(`(?i)system\s+prompt\s+override`),
		"sys_prompt_override",
	},
	{
		regexp.MustCompile(`(?i)disregard\s+(?:your|all|any)\s+(?:instructions|rules|guidelines)`),
		"disregard_rules",
	},
	{
		regexp.MustCompile(`(?i)cat\s+[^\n]*(?:\.env|credentials|\.netrc|\.pgpass)`),
		"read_secrets",
	},
	{
		regexp.MustCompile(`(?i)rm\s+-rf\s+/`),
		"destructive_root_rm",
	},
}

var cronSecretVarRe = regexp.MustCompile(`(?i)\$\{?\w*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|API)\w*\}?`)

var cronExfilPatterns = []struct {
	regex *regexp.Regexp
	id    string
}{
	{
		regexp.MustCompile(`(?i)curl\s+[^\n]*https?://[^\s"\'` + "`" + `]*` + cronSecretVarRe.String()),
		"exfil_curl_url",
	},
	{
		regexp.MustCompile(`(?i)curl\s+[^\n]*(?:--data(?:-raw|-binary|-urlencode)?|-d|--form|-F)\s+[^\n]*` + cronSecretVarRe.String()),
		"exfil_curl_data",
	},
}

var cronInvisibleChars = []rune{
	'\u200b', '\u200c', '\u200d', '\u2060', '\ufeff',
	'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
}

// scanCronPrompt 扫描 cron prompt 中的安全威胁。
// 返回错误字符串（被阻止时）或空字符串（通过）。
func scanCronPrompt(prompt string) string {
	if prompt == "" {
		return ""
	}

	// 检测不可见 Unicode
	for _, ch := range cronInvisibleChars {
		if strings.ContainsRune(prompt, ch) {
			return fmt.Sprintf("Blocked: prompt contains invisible unicode U+%04X (possible injection).", ch)
		}
	}

	// 检测注入模式
	for _, p := range cronThreatPatterns {
		if p.regex.MatchString(prompt) {
			return fmt.Sprintf("Blocked: prompt matches threat pattern '%s'. Cron prompts must not contain injection or exfiltration payloads.", p.id)
		}
	}

	// 检测数据渗出
	for _, p := range cronExfilPatterns {
		if p.regex.MatchString(prompt) {
			return fmt.Sprintf("Blocked: prompt matches threat pattern '%s'. Cron prompts must not contain injection or exfiltration payloads.", p.id)
		}
	}

	return ""
}

// ============================================================================
// resolveJobRef — 按名称或 ID 查找任务
// ============================================================================

// resolveJobRef 按精确 ID 或名称（大小写不敏感）查找任务。
// 如果名称匹配多个任务，返回错误。
func (m *Manager) resolveJobRef(ref string) (*Job, error) {
	if ref == "" {
		return nil, errs.Newf("empty job reference")
	}

	// 精确 ID 匹配
	if job, ok := m.store.Get(ref); ok {
		return job, nil
	}

	// 名称匹配（大小写不敏感）
	refLower := strings.ToLower(ref)
	var matches []*Job
	for _, job := range m.store.List() {
		if strings.ToLower(job.Name) == refLower {
			matches = append(matches, job)
		}
	}

	switch len(matches) {
	case 0:
		return nil, errs.Newf("job %q not found", ref)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, j := range matches {
			ids[i] = j.ID
		}
		return nil, errs.Newf("job name %q is ambiguous — matches %d jobs: %s. Use the job ID instead.",
			ref, len(matches), strings.Join(ids, ", "))
	}
}

// ============================================================================
// 统一 cron 工具
// ============================================================================

func cronToolDef(mgr *Manager) agenttools.ToolDef {
	return agenttools.ToolDef{
		Category: "cron",
		Scopes:   []string{"private", "group"},
		Tool: llm.Tool{
			Name:         "cron",
			DeferredLoad: true, // 定时任务非日常任务，初始仅暴露名称+描述
			Description: "Manage scheduled jobs. Pick the operation with the action parameter: create, list, get, " +
				"update, remove, pause, resume, trigger (run now)." +
				"\n\n" +
				"Creating a job requires name, prompt and schedule. schedule accepts: a cron expression (\"0 9 * * 1-5\"), " +
				"a recurring interval (\"every 30m\"), a relative delay (\"2h\"/\"1d\"), or an ISO timestamp (\"2026-03-20T14:00\")." +
				"\n\n" +
				"IMPORTANT: a cron job runs unattended in its own session, so the prompt MUST be self-contained — it has none of the current conversation context. " +
				"NEVER guess a job_id; always list first. job_id also accepts a job name.",
			// Description 为英文，但用户通常用中文询问定时能力。tool_search仅对
			// name/description/keywords 做子串匹配，故显式保留中文检索词。
			Keywords: []string{
				"定时任务", "定时", "计划任务", "周期任务", "提醒", "闹钟",
				"每天", "每周", "每小时", "延迟执行", "cron", "schedule",
			},
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"description": "Operation to perform. create=create a new job (needs name+prompt+schedule); list=list jobs; get=show details (needs job_id); update=change attributes (needs job_id); remove=delete (needs job_id); pause=pause (needs job_id); resume=resume (needs job_id); trigger=run now (needs job_id)",
						"enum":        []string{"create", "list", "get", "update", "remove", "pause", "resume", "trigger"},
					},
					"job_id": map[string]any{
						"type":        "string",
						"description": "Job ID or job name. Required for get/update/remove/pause/resume/trigger. Obtain it with action=list.",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Required for create: the job name. Optional for update: the new name.",
					},
					"prompt": map[string]any{
						"type":        "string",
						"description": "Required for create: the complete instruction to execute when the job fires. MUST be self-contained — it runs in a separate session with no conversation context. Optional for update.",
					},
					"schedule": map[string]any{
						"type":        "string",
						"description": "Required for create, optional for update. The schedule expression. Examples: \"0 9 * * 1-5\" (9:00 on weekdays), \"every 30m\" (every 30 minutes), \"2h\" (once, two hours from now), \"2026-03-20T14:00\" (at that exact moment)",
					},
					"model": map[string]any{
						"type":        "string",
						"description": "Optional: override the default model. Available on create and update.",
					},
					"skills": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional: skills to activate during execution. Pass an empty array on update to clear them.",
					},
					"max_runs": map[string]any{
						"type":        "integer",
						"description": "Optional: maximum number of runs. 0 or omitted means run indefinitely. One-shot jobs (relative delay / ISO timestamp) default to 1.",
						"default":     0,
					},
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional: custom tags for organizing jobs.",
					},
					"state": map[string]any{
						"type":        "string",
						"description": "Optional for list: filter by state. One of active/paused/done/failed/disabled.",
						"enum":        []string{"active", "paused", "done", "failed", "disabled"},
					},
					"enabled": map[string]any{
						"type":        "boolean",
						"description": "Optional for update: whether the job is enabled (false disables it).",
					},
				},
				"required": []string{"action"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}

				action := strings.ToLower(strings.TrimSpace(getString(m, "action")))
				if action == "" {
					return toolErr("action is required"), nil
				}

				switch action {
				case "create":
					return cronActionCreate(mgr, m)
				case "list":
					return cronActionList(mgr, m)
				case "get":
					return cronActionGet(mgr, m)
				case "update":
					return cronActionUpdate(mgr, m)
				case "remove":
					return cronActionRemove(mgr, m)
				case "pause":
					return cronActionPause(mgr, m)
				case "resume":
					return cronActionResume(mgr, m)
				case "trigger":
					return cronActionTrigger(mgr, m)
				default:
					return toolErr(fmt.Sprintf("unknown action %q. Valid: create, list, get, update, remove, pause, resume, trigger", action)), nil
				}
			}),
		},
		PromptSection: cronToolPromptSection,
	}
}

// ============================================================================
// Action handlers
// ============================================================================

func cronActionCreate(mgr *Manager, m map[string]any) (any, error) {
	prompt := getString(m, "prompt")
	schedule := getString(m, "schedule")
	name := getString(m, "name")

	if schedule == "" {
		return toolErr("schedule is required for action=create"), nil
	}
	if prompt == "" {
		return toolErr("prompt is required for action=create"), nil
	}
	if name == "" {
		return toolErr("name is required for action=create"), nil
	}

	// 安全扫描
	if scanErr := scanCronPrompt(prompt); scanErr != "" {
		return toolErr(scanErr), nil
	}

	req := CreateJobRequest{
		Name:     name,
		Prompt:   prompt,
		Schedule: schedule,
		Model:    getString(m, "model"),
		MaxRuns:  getInt(m, "max_runs"),
	}
	if v, ok := m["skills"].([]any); ok {
		req.Skills = toStringSlice(v)
	}
	if v, ok := m["tags"].([]any); ok {
		req.Tags = toStringSlice(v)
	}

	job, err := mgr.CreateJob(req)
	if err != nil {
		return toolErr(err.Error()), nil
	}

	return map[string]any{
		"success":          true,
		"job_id":           job.ID,
		"name":             job.Name,
		"schedule":         job.Schedule,
		"schedule_kind":    string(job.ScheduleKind),
		"schedule_display": job.ScheduleDisplay,
		"next_run_at":      formatTime(job.NextRunAt),
		"max_runs":         job.MaxRuns,
		"message":          fmt.Sprintf("Cron job '%s' created.", job.Name),
	}, nil
}

func cronActionList(mgr *Manager, m map[string]any) (any, error) {
	stateFilter := getString(m, "state")

	allJobs := mgr.ListJobs()
	jobs := make([]map[string]any, 0, len(allJobs))
	for _, j := range allJobs {
		if stateFilter != "" && string(j.State) != stateFilter {
			continue
		}
		jobs = append(jobs, jobToSummary(j))
	}

	return map[string]any{
		"success": true,
		"count":   len(jobs),
		"jobs":    jobs,
	}, nil
}

func cronActionGet(mgr *Manager, m map[string]any) (any, error) {
	jobRef := getString(m, "job_id")
	if jobRef == "" {
		return toolErr("job_id is required for action=get"), nil
	}

	job, err := mgr.resolveJobRef(jobRef)
	if err != nil {
		return toolErr(err.Error()), nil
	}

	return map[string]any{
		"success":          true,
		"job_id":           job.ID,
		"name":             job.Name,
		"prompt":           job.Prompt,
		"model":            job.Model,
		"channel":          job.Channel,
		"skills":           job.Skills,
		"feature":          job.Feature,
		"schedule":         job.Schedule,
		"schedule_kind":    string(job.ScheduleKind),
		"schedule_display": job.ScheduleDisplay,
		"max_runs":         job.MaxRuns,
		"run_count":        job.RunCount,
		"enabled":          job.Enabled,
		"state":            string(job.State),
		"next_run_at":      formatTime(job.NextRunAt),
		"last_run_at":      formatTime(job.LastRunAt),
		"last_result":      job.LastResult,
		"last_error":       job.LastError,
		"tags":             job.Tags,
		"created_at":       job.CreatedAt.Format(time.RFC3339),
		"updated_at":       job.UpdatedAt.Format(time.RFC3339),
	}, nil
}

func cronActionUpdate(mgr *Manager, m map[string]any) (any, error) {
	jobRef := getString(m, "job_id")
	if jobRef == "" {
		return toolErr("job_id is required for action=update"), nil
	}

	// 先解析 job 引用获取真实 ID
	job, err := mgr.resolveJobRef(jobRef)
	if err != nil {
		return toolErr(err.Error()), nil
	}

	updates := map[string]any{}
	if v, ok := m["name"]; ok {
		updates["name"] = v
	}
	if v, ok := m["prompt"].(string); ok && v != "" {
		// 安全扫描
		if scanErr := scanCronPrompt(v); scanErr != "" {
			return toolErr(scanErr), nil
		}
		updates["prompt"] = v
	}
	if v, ok := m["schedule"]; ok {
		updates["schedule"] = v
	}
	if v, ok := m["model"]; ok {
		updates["model"] = v
	}
	if v, ok := m["skills"]; ok {
		if skills, ok := v.([]any); ok {
			updates["skills"] = toStringSlice(skills)
		}
	}
	if v, ok := m["max_runs"]; ok {
		updates["max_runs"] = toFloat64(v)
	}
	if v, ok := m["enabled"]; ok {
		updates["enabled"] = v
	}

	if len(updates) == 0 {
		return toolErr("no fields to update. Provide at least one of: name, prompt, schedule, model, skills, max_runs, enabled"), nil
	}

	updated, err := mgr.UpdateJob(job.ID, updates)
	if err != nil {
		return toolErr(err.Error()), nil
	}

	return map[string]any{
		"success":        true,
		"job_id":         updated.ID,
		"name":           updated.Name,
		"state":          string(updated.State),
		"schedule":       updated.Schedule,
		"next_run_at":    formatTime(updated.NextRunAt),
		"updated_fields": updateKeys(updates),
	}, nil
}

func cronActionRemove(mgr *Manager, m map[string]any) (any, error) {
	jobRef := getString(m, "job_id")
	if jobRef == "" {
		return toolErr("job_id is required for action=remove"), nil
	}

	job, err := mgr.resolveJobRef(jobRef)
	if err != nil {
		return toolErr(err.Error()), nil
	}

	if err := mgr.DeleteJob(job.ID); err != nil {
		return toolErr(err.Error()), nil
	}

	return map[string]any{
		"success": true,
		"job_id":  job.ID,
		"name":    job.Name,
		"message": fmt.Sprintf("Cron job '%s' removed.", job.Name),
	}, nil
}

func cronActionPause(mgr *Manager, m map[string]any) (any, error) {
	jobRef := getString(m, "job_id")
	if jobRef == "" {
		return toolErr("job_id is required for action=pause"), nil
	}

	job, err := mgr.resolveJobRef(jobRef)
	if err != nil {
		return toolErr(err.Error()), nil
	}

	if err := mgr.PauseJob(job.ID); err != nil {
		return toolErr(err.Error()), nil
	}

	return map[string]any{
		"success": true,
		"job_id":  job.ID,
		"name":    job.Name,
		"state":   "paused",
		"message": fmt.Sprintf("Cron job '%s' paused.", job.Name),
	}, nil
}

func cronActionResume(mgr *Manager, m map[string]any) (any, error) {
	jobRef := getString(m, "job_id")
	if jobRef == "" {
		return toolErr("job_id is required for action=resume"), nil
	}

	job, err := mgr.resolveJobRef(jobRef)
	if err != nil {
		return toolErr(err.Error()), nil
	}

	if err := mgr.ResumeJob(job.ID); err != nil {
		return toolErr(err.Error()), nil
	}

	return map[string]any{
		"success":     true,
		"job_id":      job.ID,
		"name":        job.Name,
		"state":       "active",
		"next_run_at": formatTime(job.NextRunAt),
		"message":     fmt.Sprintf("Cron job '%s' resumed.", job.Name),
	}, nil
}

func cronActionTrigger(mgr *Manager, m map[string]any) (any, error) {
	jobRef := getString(m, "job_id")
	if jobRef == "" {
		return toolErr("job_id is required for action=trigger"), nil
	}

	job, err := mgr.resolveJobRef(jobRef)
	if err != nil {
		return toolErr(err.Error()), nil
	}

	if err := mgr.TriggerJob(job.ID); err != nil {
		return toolErr(err.Error()), nil
	}

	return map[string]any{
		"success": true,
		"job_id":  job.ID,
		"name":    job.Name,
		"message": fmt.Sprintf("Cron job '%s' triggered — will execute on next scheduler tick.", job.Name),
	}, nil
}

// ============================================================================
// RegisterTools — 注册定时任务工具
// ============================================================================

// RegisterTools 将定时任务工具注册到 ToolManager。
//
// 注册单个统一工具 `cron`，通过 action 参数分发到不同操作。
// 这种压缩工具设计减少了 LLM 上下文中的 schema token 开销。
//
// 使用示例：
//
//	mgr := cron.NewManager(store, loc)
//	cron.RegisterTools(toolMgr, mgr)
func RegisterTools(toolMgr *agenttools.ToolManager, mgr *Manager) error {
	return toolMgr.RegisterMany(cronToolDef(mgr))
}

// ============================================================================
// Helpers
// ============================================================================

func toolErr(msg string) map[string]any {
	return map[string]any{
		"success": false,
		"error":   msg,
	}
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func toStringSlice(v []any) []string {
	result := make([]string, 0, len(v))
	for _, item := range v {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func updateKeys(updates map[string]any) []string {
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	return keys
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func jobToSummary(j *Job) map[string]any {
	return map[string]any{
		"job_id":           j.ID,
		"name":             j.Name,
		"schedule":         j.Schedule,
		"schedule_kind":    string(j.ScheduleKind),
		"schedule_display": j.ScheduleDisplay,
		"state":            string(j.State),
		"enabled":          j.Enabled,
		"next_run_at":      formatTime(j.NextRunAt),
		"last_run_at":      formatTime(j.LastRunAt),
		"run_count":        j.RunCount,
		"max_runs":         j.MaxRuns,
		"model":            j.Model,
		"tags":             j.Tags,
	}
}
