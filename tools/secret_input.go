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
// secret_input — 向用户索取密钥，LLM 全程只见占位符
//
// 两段式、非阻塞（贴合 IM 主场景：工具无法在企微/TG 主动弹原生输入卡）：
//  1. LLM 调用本工具并给出 purpose（用途说明）；工具注册一个 pending 密钥请求，
//     立即返回占位符 {{secret:name}}，不等待。
//  2. LLM 据此向用户发一条消息，说明用途并请其直接回复密钥。
//  3. 用户在 IM 回复明文 → Ingress.Receive 前置拦截（consumeSecretReply）：
//     明文写入保险库，消息正文被改写为脱敏的「密钥已就绪」提示后才投递 pipeline。
//  4. LLM 收到脱敏提示，在后续工具参数中写占位符；唯一执行咽喉点自动还原。
//
// 明文在任何时刻都不进入 LLM 上下文、日志、聊天历史与记忆。
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
				"(2) Then send the user a short message stating the purpose and asking them to reply with the secret. " +
				"When the user replies, the system stores it securely and you get a REDACTED notice that the placeholder is ready — you never receive the raw value. " +
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
- The tool returns a placeholder such as {{secret:name}}. It does NOT block. After calling it, send the user a short message stating the purpose and asking them to reply with the secret.
- The user's reply is handled securely by the system: the raw value is stored in a vault and NEVER enters your context. You will get a REDACTED notice when the placeholder is ready.
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

	meta := agenttools.MessageMetaFromContext(ctx.Context)
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
		ChatID:      meta.ChatID,
		Question:    purpose,
		Kind:        interaction.KindSecret,
		SecretName:  name,
		TimeoutSecs: timeout,
	}
	if _, err := interaction.Default().RegisterQuestion(q); err != nil {
		return nil, fmt.Errorf("secret_input: 注册请求失败: %w", err)
	}

	return map[string]any{
		"status":      "requested",
		"name":        name,
		"placeholder": secret.Placeholder(name),
		"purpose":     purpose,
		"expires_in":  timeout,
		"hint":        "已登记密钥请求。请现在向用户发一条消息，说明用途并请其直接回复该密钥；用户回复后系统安全存储，你会收到一条脱敏的「密钥已就绪」提示，届时再用占位符继续任务。不要在本回合索要或猜测明文。",
	}, nil
}
