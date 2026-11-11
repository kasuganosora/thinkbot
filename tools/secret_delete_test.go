package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/secret"
)

func withSecretToolVault(t *testing.T) *secret.Vault {
	t.Helper()
	v, err := secret.New("")
	if err != nil {
		t.Fatal(err)
	}
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })
	return v
}

func secretDeleteCtx(botID string) *llm.ToolExecContext {
	return &llm.ToolExecContext{Context: agenttools.ContextWithMessageMeta(
		context.Background(),
		agenttools.MessageMeta{BotID: botID, ChatID: "chat-test"},
	)}
}

func callSecretDelete(t *testing.T, ctx *llm.ToolExecContext, input any) map[string]any {
	t.Helper()
	out, err := secretDeleteToolDef().Tool.Execute(ctx, input)
	if err != nil {
		t.Fatalf("secret_delete failed: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", out)
	}
	return m
}

func TestSecretDelete_Validation(t *testing.T) {
	withSecretToolVault(t)
	execute := secretDeleteToolDef().Tool.Execute
	if _, err := execute(secretDeleteCtx("bot-a"), map[string]any{"name": "  "}); err == nil {
		t.Fatal("empty name should be rejected")
	}
	if _, err := execute(&llm.ToolExecContext{}, map[string]any{"name": "k"}); err == nil {
		t.Fatal("missing context should be rejected")
	}
	if _, err := execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{"name": "k"}); err == nil {
		t.Fatal("missing bot identity should be rejected")
	}
	if _, err := execute(secretDeleteCtx("bot-a"), map[string]any{"name": "bad name"}); err == nil {
		t.Fatal("invalid name should be rejected by the vault")
	}
}

func TestSecretDelete_DeletesOwnEntryWithoutLeakingValue(t *testing.T) {
	v := withSecretToolVault(t)
	const value = "credential-that-must-never-be-returned"
	if err := v.Register("bot-a", "svc.token", value, "示例用途"); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("bot-b", "svc.token", "another-bots-value-long", ""); err != nil {
		t.Fatal(err)
	}

	res := callSecretDelete(t, secretDeleteCtx("bot-a"), map[string]any{"name": "svc.token"})
	if res["status"] != string(secret.DeleteStatusDeleted) || res["scope"] != secret.ScopeBot {
		t.Fatalf("unexpected result: %+v", res)
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), value) {
		t.Fatalf("tool result leaked the credential value: %s", encoded)
	}
	if _, ok := v.Get("bot-a", "svc.token"); ok {
		t.Fatal("entry still resolvable after delete")
	}
	if got, ok := v.Get("bot-b", "svc.token"); !ok || got != "another-bots-value-long" {
		t.Fatalf("other bot's entry damaged: %q %v", got, ok)
	}
}

func TestSecretDelete_GlobalRequiresExplicitConfirm(t *testing.T) {
	v := withSecretToolVault(t)
	if err := v.Register("", "shared.key", "globally-shared-value-long", ""); err != nil {
		t.Fatal(err)
	}
	res := callSecretDelete(t, secretDeleteCtx("bot-a"), map[string]any{"name": "shared.key"})
	if res["status"] != string(secret.DeleteStatusConfirmRequired) || res["scope"] != secret.ScopeGlobal {
		t.Fatalf("global name must require confirmation: %+v", res)
	}
	if _, ok := v.Get("bot-a", "shared.key"); !ok {
		t.Fatal("global entry removed without confirmation")
	}
	res = callSecretDelete(t, secretDeleteCtx("bot-a"), map[string]any{
		"name": "shared.key", "allow_global": true,
	})
	if res["status"] != string(secret.DeleteStatusDeleted) {
		t.Fatalf("confirmed delete failed: %+v", res)
	}
	if _, ok := v.Get("bot-a", "shared.key"); ok {
		t.Fatal("global entry still resolvable")
	}
}

func TestSecretDelete_NotFound(t *testing.T) {
	withSecretToolVault(t)
	res := callSecretDelete(t, secretDeleteCtx("bot-a"), map[string]any{"name": "missing"})
	if res["status"] != string(secret.DeleteStatusNotFound) {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSecretDelete_RegisteredAsTool(t *testing.T) {
	mgr := newTestToolMgr(t)
	if err := RegisterTools(mgr, Config{}); err != nil {
		t.Fatal(err)
	}
	tools, err := mgr.ResolveTools(context.Background(), &agenttools.ToolSessionContext{
		BotID: "test-bot", ChatType: "private",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Name == "secret_delete" {
			return
		}
	}
	t.Fatal("secret_delete not exposed to the model")
}
