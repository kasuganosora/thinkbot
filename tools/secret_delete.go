package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/secret"
)

// ============================================================================
// secret_delete — 让 bot 能按名字删除已登记的凭据
//
// 与 secret_input 的 action=list 成对：list 只给名字/用途，delete 只吃名字，
// 两端都碰不到明文值。删除按「本 bot 可见」的语义解析（本 bot 条目优先，
// 回落全局条目）；全局条目由所有 bot 共享，需 allow_global 显式确认。
// ============================================================================

type secretDeleteArgs struct {
	Name        string `json:"name"`
	AllowGlobal bool   `json:"allow_global,omitempty"`
}

func secretDeleteToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Tool: llm.Tool{
			Name: "secret_delete",
			Description: "Permanently delete a stored credential by name. " +
				"Call secret_input with action=list first to get the exact name; this tool never returns a secret value. " +
				"Deletion is irreversible, so use it only when the user explicitly asks to remove/revoke a credential, or to discard one that was just stored by mistake. " +
				"By default it deletes the entry owned by this bot. If a name only exists as a shared (global) entry, the tool returns status=confirm_required instead of deleting: tell the user that removing it affects every bot, and retry with allow_global=true only after they confirm. " +
				"Returns status: deleted / confirm_required / not_found, plus the scope that was affected.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Exact credential name as shown by secret_input action=list (letters/digits/dot/underscore/hyphen).",
					},
					"allow_global": map[string]any{
						"type":        "boolean",
						"description": "Set true ONLY after the user explicitly confirms deleting a shared/global credential visible to every bot. Default false.",
					},
				},
				"required": []string{"name"},
			},
			Execute: llm.ToolExecuteFunc(execSecretDelete),
		},
		Category:      "utility",
		Scopes:        []string{"private"},
		PromptSection: secretDeletePromptSection,
	}
}

var secretDeletePromptSection = &agenttools.ToolPromptSection{
	Name:    "secret_delete",
	Order:   323,
	Enabled: true,
	Content: `## Deleting credentials

- To remove a credential that is no longer needed, was stored by mistake, or must be revoked, call **secret_delete** with the exact name from secret_input action=list. Deletion is permanent: only do it when the user asks for it, and state the name back to the user.
- After a successful delete, the placeholder for that name no longer resolves. If the task still needs the credential, ask the user for a new one with secret_input action=request.
- If the result says status=confirm_required, the entry is shared by all bots. Explain that before retrying with allow_global=true, and never set that flag without the user's confirmation.
- If the result says status=not_found, do not guess other names; list the stored credentials again.`,
}

func execSecretDelete(ctx *llm.ToolExecContext, input any) (any, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("secret_delete: failed to marshal input: %w", err)
	}
	var in secretDeleteArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("secret_delete: invalid input format: %w", err)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("secret_delete: name is required")
	}
	if ctx == nil || ctx.Context == nil {
		return nil, fmt.Errorf("secret_delete: missing conversation context")
	}
	meta := agenttools.MessageMetaFromContext(ctx.Context)
	if meta.BotID == "" {
		return nil, fmt.Errorf("secret_delete: missing bot identity")
	}

	result, err := secret.Default().Delete(meta.BotID, name, in.AllowGlobal)
	if err != nil {
		return nil, fmt.Errorf("secret_delete: %w", err)
	}
	// 删除是不可逆操作，留一条不含明文值的审计线。
	zap.S().Infow("secret_delete: 凭据删除请求",
		"bot_id", meta.BotID, "name", name,
		"status", string(result.Status), "scope", result.Scope,
		"allow_global", in.AllowGlobal)
	out := map[string]any{
		"status": string(result.Status),
		"name":   result.Name,
	}
	switch result.Status {
	case secret.DeleteStatusDeleted:
		out["scope"] = result.Scope
		if result.RevealedGlobal {
			out["hint"] = "Deleted this bot's credential; a global shared credential with the same name still exists, so this name now resolves to the global value. If the user wants it removed completely, confirm with the user first and call again with allow_global=true."
		} else if result.Scope == secret.ScopeGlobal {
			out["hint"] = "Deleted the global shared credential; no bot can access this name anymore."
		} else {
			out["hint"] = "Credential permanently deleted; its placeholder no longer resolves. Request a new one via secret_input if still needed."
		}
	case secret.DeleteStatusConfirmRequired:
		out["scope"] = secret.ScopeGlobal
		out["hint"] = "This name refers to a global shared credential and deleting it affects all bots. Explain this to the user and obtain confirmation before calling again with allow_global=true."
	case secret.DeleteStatusNotFound:
		out["hint"] = "No credential found with that name; call secret_input with action=list to inspect stored names instead of guessing."
	}
	return out, nil
}
