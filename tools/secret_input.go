package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/secret"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// secret_input — 通过专用输入流程向用户索取密钥，向 LLM 返回占位符
//
// 两段式、非阻塞：
//  1. LLM 调用本工具并给出 purpose（用途说明）；工具注册一个 pending 密钥请求，
//     立即返回占位符 {{secret:name}}，不等待。
//  2. Web 通过绑定用户、机器人、会话和请求 ID 的密码卡提交；失效请求直接拒绝。
//     IM 使用下一条回复拦截，只有内存中的请求仍存在时才能识别密钥回复。
//  3. 专用输入路径把明文写入保险库，仅投递脱敏的「密钥已就绪」提示。
//  4. LLM 收到脱敏提示，在后续工具参数中写占位符；唯一执行咽喉点自动还原。
//
// 普通聊天正文不是安全凭据输入。服务重启会丢失 pending 请求；Web 卡片可拒绝
// 旧 ID，IM 则需用户先重新请求，不能保证未知凭据在普通聊天中被自动识别。
// ============================================================================

type secretInputArgs struct {
	Action      string `json:"action"`
	Purpose     string `json:"purpose,omitempty"`
	Name        string `json:"name,omitempty"`
	TimeoutSecs int    `json:"timeout_secs,omitempty"`
}

func secretInputToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "secret_input",
			Description: "Request a secret credential (API token, password, key) from the user WITHOUT you ever seeing its value. " +
				"Two-step, non-blocking: (1) call this with action='request' and a 'purpose' describing where/why the secret is needed, plus optional 'name'; " +
				"the tool returns immediately with a placeholder like {{secret:name}} instead of blocking. " +
				"(2) Follow the returned input_mode: for web_form, direct the user to the password card, never the chat composer. For chat_reply, the next non-empty reply is treated as a secret, including questions; /cancel-secret cancels. After a restart or if uncertain, request a new input before submitting. " +
				"When the user replies, the system returns a REDACTED notice when the placeholder is ready. " +
				"Then continue the task by putting the placeholder in tool arguments (e.g. an Authorization header); it is resolved to the real value only at execution time. " +
				"action='list' returns the names/purposes of already-stored secrets (never their values). " +
				"Rules: state the purpose clearly and honestly; NEVER ask the user to paste the secret in a way you would read back verbatim; " +
				"NEVER guess, echo, or try to reconstruct the real value; if the placeholder is not ready yet, wait for the user's reply instead of proceeding. " +
				"This is a sensitive capability — only request a secret when the task genuinely requires one the user must provide.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"request", "list"},
						"description": "request (default) asks the user for a new secret; list shows stored secret names/purposes.",
					},
					"purpose": map[string]any{
						"type":        "string",
						"description": "Required for action=request. Explain to the user WHERE and WHY this secret will be used (e.g. 'to call the internal wiki API on your behalf'). Shown to the user so they can decide.",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Optional short key name for action=request (letters/digits/dot/underscore/hyphen). Auto-generated if omitted. The placeholder becomes {{secret:<name>}}.",
					},
					"timeout_secs": map[string]any{
						"type":        "integer",
						"description": "How long a reply is treated as the secret, default 600s.",
					},
				},
			},
			Execute: llm.ToolExecuteFunc(execSecretInput),
		},
		Category:      "utility",
		Scopes:        []string{"private"},
		PromptSection: secretInputPromptSection,
	}
}

var secretInputPromptSection = &agenttools.ToolPromptSection{
	Name:    "secret_input",
	Order:   322,
	Enabled: true,
	Content: `## Secrets and credentials

- When a task needs a credential the user must provide (API token, password, key), call **secret_input** with action=request and a clear **purpose** describing where/why it will be used.
- The tool returns a placeholder and an input_mode. It does NOT block. For web_form, explain the purpose and direct the user to the password card, NEVER the ordinary chat composer. For chat_reply, explain that the next non-empty reply, including a question, is treated as the secret; /cancel-secret cancels. Only one request may be pending per conversation. After a restart or if the request status is uncertain, request a new input before submitting.
- The user's reply is handled securely by the system: the supported input path stores the raw value in the vault and returns only a redacted notice. Do not claim that ordinary chat text is a secure credential input. You will get a REDACTED notice when the placeholder is ready.
- To use a stored secret, write its placeholder {{secret:name}} in a tool argument (URL, header, body, command, etc.). It is resolved to the real value only at execution time — you never see the real value.
- Use action=list to see which secret names/purposes are already stored (values are never shown).
- NEVER guess, echo, reconstruct, or ask the user to paste a secret in a way you would read verbatim. If a placeholder is not ready, wait for the user's reply.
- This is a sensitive capability: only request a secret when the task genuinely needs one.`,
}

func execSecretInput(ctx *llm.ToolExecContext, input any) (any, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("secret_input: 无法序列化入参: %w", err)
	}
	var in secretInputArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("secret_input: 入参格式错误: %w", err)
	}
	action := strings.TrimSpace(strings.ToLower(in.Action))
	if action == "" {
		action = "request"
	}

	if ctx == nil || ctx.Context == nil {
		return nil, fmt.Errorf("secret_input: missing conversation context")
	}
	meta := agenttools.MessageMetaFromContext(ctx.Context)
	if meta.BotID == "" || meta.ChatID == "" {
		return nil, fmt.Errorf("secret_input: missing bot or conversation identity")
	}
	if action == "list" {
		// 只列本 bot 可见的条目（本 bot + 全局），别的 bot 的目录不外露。
		return map[string]any{
			"status":  "ok",
			"secrets": secret.Default().Catalog(meta.BotID),
		}, nil
	}
	if action != "request" {
		return nil, fmt.Errorf("secret_input: 未知 action %q", action)
	}

	chatID := interaction.SecretReplyChatID(meta.ChatID, agenttools.CallOriginFromContext(ctx.Context).SessionID)
	if chatID == "" {
		return nil, fmt.Errorf("secret_input: missing web conversation identity")
	}
	purpose := strings.TrimSpace(in.Purpose)
	if purpose == "" {
		return nil, fmt.Errorf("secret_input: purpose 不能为空（需向用户说明用途）")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "secret." + idgen.New("s")
	}
	// 名字合法性由保险库 Register 再次校验；此处先拦掉会破坏占位符语法的字符。
	if strings.ContainsAny(name, "{} \t\n") || strings.HasPrefix(name, "secret:") {
		return nil, fmt.Errorf("secret_input: 非法 name %q", name)
	}

	timeout := in.TimeoutSecs
	if timeout <= 0 {
		timeout = interaction.DefaultTimeoutSecs
	}
	questionID := idgen.New("si")
	q := interaction.Question{
		ID:          questionID,
		BotID:       meta.BotID,
		ChatID:      chatID,
		Question:    purpose,
		Kind:        interaction.KindSecret,
		SecretName:  name,
		SecretForm:  strings.HasPrefix(meta.ChatID, "web:"),
		TimeoutSecs: timeout,
	}
	q, err = interaction.Default().RegisterQuestion(q)
	if err != nil {
		return nil, fmt.Errorf("secret_input: 注册请求失败: %w", err)
	}

	hint := "已登记密钥请求。请说明用途并请用户回复该密钥；可发送 /cancel-secret 取消。若服务重启或不确定请求是否仍有效，应重新发起请求，不要直接贴凭据。"
	mode := "chat_reply"
	if q.SecretForm {
		mode = "web_form"
		hint = "已显示专用密钥输入卡。请说明用途，引导用户只在卡片密码框中提交，不要在聊天正文粘贴凭据。卡片失效或服务重启后必须重新请求；收到密钥就绪提示后再继续任务。"
	}
	return map[string]any{
		"question_id": questionID,
		"input_mode":  mode,
		"expires_at":  q.RegisteredAt + int64(q.TimeoutSecs),
		"status":      "requested",
		"name":        name,
		"placeholder": secret.Placeholder(name),
		"purpose":     purpose,
		"expires_in":  timeout,
		"hint":        hint,
	}, nil
}
