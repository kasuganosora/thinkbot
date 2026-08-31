package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/llm/openai"
	"github.com/kasuganosora/thinkbot/util/log"
)

// ============================================================================
// 真实 MCP + LLM API 集成测试
//
// 使用智谱 BigModel 的 web-search-prime MCP 服务器 + GLM-5.2 LLM
// 验证完整链路：
//
//	MCP Server (web-search-prime)
//	  → Manager.Connect
//	  → Provider.Tools (缓存)
//	  → ToolManager.ResolveTools
//	  → OrchestrateGenerate (MaxSteps > 0)
//	  → GLM-5.2 决策调用 MCP 搜索工具
//	  → MCP 服务器执行搜索
//	  → 结果回传 LLM
//	  → 最终回复
//
// 运行命令（需先配置 mcp/.env.test，否则用例自动 Skip）：
//
//	cp mcp/.env.test.example mcp/.env.test   # 填入自己的凭据
//	go test -v -run TestIntegration ./mcp/ -timeout 180s
// ============================================================================

// 这些集成测试依赖**真实的外部 MCP 服务器与 LLM API**。凭据一律从测试 .env
// 读取，**不得硬编码进源码**（此前本文件提交过真实 token/API key）。
//
// 配置来源（优先级从高到低）：
//  1. 进程环境变量；
//  2. mcp/.env.test；
//  3. 仓库根 .env。
//
// 三者都没有凭据时，相关用例整体 Skip —— 这样 `go test ./...` 不会因缺少外部
// 依赖而失败（此前会报 "parse initialize result: unexpected end of JSON input"）。
//
// 用法：cp mcp/.env.test.example mcp/.env.test 后填入自己的凭据。
// 两个 .env 文件均已被 .gitignore 排除。
const (
	envKeyMCPServerName = "MCP_INTEG_SERVER_NAME"
	envKeyMCPURL        = "MCP_INTEG_URL"
	envKeyMCPAuthToken  = "MCP_INTEG_AUTH_TOKEN"
	envKeyLLMAPIKey     = "MCP_INTEG_LLM_API_KEY"
	envKeyLLMBaseURL    = "MCP_INTEG_LLM_BASE_URL"
	envKeyLLMChatPath   = "MCP_INTEG_LLM_CHAT_PATH"
	envKeyLLMModel      = "MCP_INTEG_LLM_MODEL"
)

var (
	// MCP web-search-prime 服务器（端点/名称属非敏感项，保留默认值）
	integMCPServerName = integEnv(envKeyMCPServerName, "web-search-prime")
	integMCPURL        = integEnv(envKeyMCPURL, "https://open.bigmodel.cn/api/mcp/web_search_prime/mcp")
	// 凭据无默认值：缺失即跳过测试。
	integMCPAuthToken = integEnv(envKeyMCPAuthToken, "")

	// LLM API
	integLLMAPIKey   = integEnv(envKeyLLMAPIKey, "")
	integLLMBaseURL  = integEnv(envKeyLLMBaseURL, "https://open.bigmodel.cn/api/coding/paas")
	integLLMChatPath = integEnv(envKeyLLMChatPath, "/v4/chat/completions")
	integLLMModel    = integEnv(envKeyLLMModel, "glm-5.2")
)

// integEnvFiles 惰性加载测试 .env，仅解析一次。
// 后出现的文件不覆盖先出现的键，因此 mcp/.env.test 优先于仓库根 .env。
var integEnvFiles = sync.OnceValue(func() map[string]string {
	merged := make(map[string]string)
	for _, path := range []string{".env.test", filepath.Join("..", ".env")} {
		values, err := config.LoadEnvFile(path)
		if err != nil {
			// 测试辅助路径：读取失败不致命，退化为「无该配置源」。
			continue
		}
		for k, v := range values {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}
	}
	return merged
})

// integEnv 按「进程环境变量 → 测试 .env → 默认值」的优先级取配置。
func integEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := integEnvFiles()[key]; v != "" {
		return v
	}
	return def
}

// skipIfShort 在以下任一情况下跳过集成测试：
//   - `-short` 模式；
//   - 未配置 MCP 凭据（默认即此，故 `go test ./...` 不会被外部依赖拖垮）。
//
// 明确提示缺哪个配置项，而不是让请求发出去后报难懂的解析错误。
func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if integMCPAuthToken == "" {
		t.Skipf("skipping MCP integration test: %s not set "+
			"(put it in mcp/.env.test — see mcp/.env.test.example)", envKeyMCPAuthToken)
	}
}

// skipIfNoLLM 供额外依赖 LLM API 的用例使用（MCP 凭据之外还需 LLM key）。
func skipIfNoLLM(t *testing.T) {
	t.Helper()
	if integLLMAPIKey == "" {
		t.Skipf("skipping MCP+LLM integration test: %s not set "+
			"(put it in mcp/.env.test — see mcp/.env.test.example)", envKeyLLMAPIKey)
	}
}

// setupIntegrationMCP 创建并连接到真实的 web-search-prime MCP 服务器。
// 返回 Manager，调用方负责 Close。
func setupIntegrationMCP(t *testing.T) *Manager {
	t.Helper()

	mgr := NewManager(zap.NewNop().Sugar())
	mgr.AddServer(ServerConfig{
		Name:      integMCPServerName,
		Transport: "http",
		URL:       integMCPURL,
		Headers: map[string]string{
			"Authorization": integMCPAuthToken,
		},
		Enabled: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mgr.Connect(ctx); err != nil {
		t.Fatalf("Manager.Connect: %v", err)
	}

	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

// ============================================================================
// 测试 1：MCP 握手 + 工具列表
// ============================================================================

func TestIntegration_MCP_Connect(t *testing.T) {
	skipIfShort(t)
	log.Logger = zap.NewNop().Sugar()

	mgr := setupIntegrationMCP(t)

	// 验证连接状态
	if !mgr.IsServerConnected(integMCPServerName) {
		t.Fatal("server should be connected")
	}

	// 列出工具
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	allTools, err := mgr.ListAllTools(ctx)
	if err != nil {
		t.Fatalf("ListAllTools: %v", err)
	}

	serverTools, ok := allTools[integMCPServerName]
	if !ok {
		t.Fatalf("no tools found for server %q in %v", integMCPServerName, allTools)
	}
	if len(serverTools) == 0 {
		t.Fatal("expected at least 1 tool from web-search-prime")
	}

	t.Logf("web-search-prime provides %d tool(s):", len(serverTools))
	for _, tool := range serverTools {
		t.Logf("  - %s: %s", tool.Name, tool.Description)
	}

	// 应该有搜索类工具
	found := false
	for _, tool := range serverTools {
		if strings.Contains(strings.ToLower(tool.Name), "search") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a search tool, got: %v", serverTools)
	}
}

// ============================================================================
// 测试 2：直接调用 MCP 工具
// ============================================================================

func TestIntegration_MCP_CallTool(t *testing.T) {
	skipIfShort(t)
	log.Logger = zap.NewNop().Sugar()

	mgr := setupIntegrationMCP(t)
	client, ok := mgr.GetClient(integMCPServerName)
	if !ok {
		t.Fatal("failed to get client")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 列出工具找到搜索工具名
	allTools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	searchToolName := ""
	for _, tool := range allTools {
		if strings.Contains(strings.ToLower(tool.Name), "search") {
			searchToolName = tool.Name
			break
		}
	}
	if searchToolName == "" {
		t.Fatalf("no search tool found in %d tools", len(allTools))
	}

	t.Logf("calling tool: %s", searchToolName)

	// 调用搜索工具
	result, err := client.CallTool(ctx, searchToolName, map[string]any{
		"search_query": "Go programming language latest version",
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if result == "" {
		t.Error("expected non-empty search result")
	}

	// 截断输出用于日志
	preview := result
	if len(preview) > 500 {
		preview = preview[:500] + "..."
	}
	t.Logf("search result preview:\n%s", preview)
}

// ============================================================================
// 测试 3：Provider → ToolManager → ResolveTools 链路
// ============================================================================

func TestIntegration_MCP_ToolResolution(t *testing.T) {
	skipIfShort(t)
	log.Logger = zap.NewNop().Sugar()

	mgr := setupIntegrationMCP(t)

	// 创建 ToolManager 并注册 MCP Provider
	toolMgr := tools.NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	if err := RegisterTools(toolMgr, mgr); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sctx := &tools.ToolSessionContext{}
	resolved, err := toolMgr.ResolveTools(ctx, sctx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if len(resolved) == 0 {
		t.Fatal("expected resolved tools, got 0")
	}

	t.Logf("resolved %d tool(s) from MCP:", len(resolved))
	for _, tool := range resolved {
		t.Logf("  - %s: %s", tool.Name, tool.Description)
	}

	// 验证工具名带服务器前缀
	prefix := integMCPServerName + "__"
	foundPrefixed := false
	for _, tool := range resolved {
		if strings.HasPrefix(tool.Name, prefix) {
			foundPrefixed = true
			break
		}
	}
	if !foundPrefixed {
		t.Errorf("expected a tool with prefix %q, got names: %v", prefix, resolved)
	}

	// 验证 Execute 闭包可用（通过直接执行搜索工具）
	for _, tool := range resolved {
		if strings.Contains(strings.ToLower(tool.Name), "search") {
			t.Logf("executing resolved tool: %s", tool.Name)
			result, err := tool.Execute(
				&llm.ToolExecContext{
					Context:  ctx,
					ToolName: tool.Name,
				},
				map[string]any{"search_query": "Rust vs Go 2025"},
			)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			str, ok := result.(string)
			if !ok || str == "" {
				t.Error("expected non-empty string result from Execute")
			}
			t.Logf("Execute succeeded, result length: %d chars", len(str))
			break
		}
	}
}

// ============================================================================
// 测试 4：真实 LLM API 调用 + MCP 工具编排
// ============================================================================

func TestIntegration_MCP_LLMOrchestration(t *testing.T) {
	skipIfShort(t)
	skipIfNoLLM(t)
	log.Logger = zap.NewNop().Sugar()

	mgr := setupIntegrationMCP(t)

	// 设置 MCP Provider
	toolMgr := tools.NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	if err := RegisterTools(toolMgr, mgr); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 解析工具
	sctx := &tools.ToolSessionContext{}
	mcpTools, err := toolMgr.ResolveTools(ctx, sctx)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if len(mcpTools) == 0 {
		t.Fatal("expected MCP tools")
	}

	// 创建 GLM LLM Provider
	maxTokens := 4096
	prov := openai.New(
		openai.WithAPIKey(integLLMAPIKey),
		openai.WithBaseURL(integLLMBaseURL),
		openai.WithChatMode(),
		openai.WithChatPath(integLLMChatPath),
		openai.WithTimeout(90*time.Second),
	)

	// 多步编排：LLM → 工具调用 → 工具执行 → 结果回传 → 最终回复
	result, err := llm.OrchestrateGenerate(ctx, prov, &llm.OrchestrateConfig{
		MaxSteps: 5,
		Params: llm.GenerateParams{
			Model:  llm.ChatModel(integLLMModel),
			System: "你是一个助手，可以搜索互联网获取最新信息。当用户询问时事或你不确定的问题时，请使用搜索工具。",
			Messages: []llm.Message{
				llm.UserMessage("2025年最新的Go语言版本是什么？有哪些主要变化？"),
			},
			Tools:     mcpTools,
			MaxTokens: &maxTokens,
		},
	})
	if err != nil {
		t.Fatalf("OrchestrateGenerate: %v", err)
	}

	// 验证 LLM 是否调用了工具
	var toolCalled bool
	for _, step := range result.Steps {
		if len(step.ToolCalls) > 0 {
			toolCalled = true
			for _, tc := range step.ToolCalls {
				t.Logf("step tool call: %s (input: %v)", tc.ToolName, tc.Input)
			}
		}
	}

	if !toolCalled {
		t.Error("expected LLM to call an MCP search tool, but it did not")
	}

	// 验证最终回复
	if result.Text == "" {
		t.Error("expected non-empty final text")
	}

	preview := result.Text
	if len(preview) > 800 {
		preview = preview[:800] + "..."
	}
	t.Logf("LLM final response:\n%s", preview)
	t.Logf("total tokens used: input=%d output=%d",
		result.Usage.InputTokens, result.Usage.OutputTokens)
}

// ============================================================================
// 测试 5：运行时 Enable/Disable 验证
// ============================================================================

func TestIntegration_MCP_EnableDisable(t *testing.T) {
	skipIfShort(t)
	log.Logger = zap.NewNop().Sugar()

	mgr := setupIntegrationMCP(t)

	// 初始状态：已启用、已连接
	if !mgr.IsServerEnabled(integMCPServerName) {
		t.Fatal("server should be enabled initially")
	}
	if !mgr.IsServerConnected(integMCPServerName) {
		t.Fatal("server should be connected initially")
	}

	// 注册到 ToolManager
	toolMgr := tools.NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	if err := RegisterTools(toolMgr, mgr); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sctx := &tools.ToolSessionContext{}

	// 解析工具：应有 MCP 工具
	resolved, err := toolMgr.ResolveTools(ctx, sctx)
	if err != nil {
		t.Fatalf("ResolveTools (before disable): %v", err)
	}
	if len(resolved) == 0 {
		t.Fatal("expected tools before disable")
	}
	t.Logf("before disable: %d tools", len(resolved))

	// 禁用服务器
	if err := mgr.DisableServer(integMCPServerName); err != nil {
		t.Fatalf("DisableServer: %v", err)
	}

	// 验证状态
	if mgr.IsServerEnabled(integMCPServerName) {
		t.Error("server should be disabled after DisableServer")
	}
	if mgr.IsServerConnected(integMCPServerName) {
		t.Error("server should be disconnected after DisableServer")
	}

	// Provider 缓存已通过 onServerChange 自动失效
	// 重新解析：应无 MCP 工具
	resolved2, err := toolMgr.ResolveTools(ctx, sctx)
	if err != nil {
		t.Fatalf("ResolveTools (after disable): %v", err)
	}
	if len(resolved2) != 0 {
		t.Errorf("expected 0 tools after disable, got %d", len(resolved2))
	}
	t.Logf("after disable: %d tools", len(resolved2))

	// 重新启用
	if err := mgr.EnableServer(ctx, integMCPServerName); err != nil {
		t.Fatalf("EnableServer: %v", err)
	}

	if !mgr.IsServerEnabled(integMCPServerName) {
		t.Error("server should be enabled after EnableServer")
	}
	if !mgr.IsServerConnected(integMCPServerName) {
		t.Error("server should be connected after EnableServer")
	}

	// 等待 Provider 缓存失效回调完成后再解析
	resolved3, err := toolMgr.ResolveTools(ctx, sctx)
	if err != nil {
		t.Fatalf("ResolveTools (after re-enable): %v", err)
	}
	if len(resolved3) == 0 {
		t.Error("expected tools after re-enable, got 0")
	}
	t.Logf("after re-enable: %d tools", len(resolved3))

	// 验证 EnableServer 后工具名一致
	for _, tool := range resolved3 {
		if !strings.HasPrefix(tool.Name, integMCPServerName+"__") {
			t.Errorf("unexpected tool name without prefix: %q", tool.Name)
		}
	}
}

// ============================================================================
// 测试 6：服务器状态快照
// ============================================================================

func TestIntegration_MCP_ListServers(t *testing.T) {
	skipIfShort(t)
	log.Logger = zap.NewNop().Sugar()

	mgr := setupIntegrationMCP(t)

	// 连接状态
	statuses := mgr.ListServers()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 server, got %d", len(statuses))
	}
	s := statuses[0]
	if s.Name != integMCPServerName {
		t.Errorf("expected name %q, got %q", integMCPServerName, s.Name)
	}
	if !s.Enabled {
		t.Error("server should be enabled")
	}
	if !s.Connected {
		t.Error("server should be connected")
	}
	if s.Transport != "http" {
		t.Errorf("expected transport 'http', got %q", s.Transport)
	}

	t.Logf("server status: %+v", s)

	// 禁用后检查状态
	_ = mgr.DisableServer(integMCPServerName)
	statuses2 := mgr.ListServers()
	if len(statuses2) != 1 {
		t.Fatalf("expected 1 server after disable, got %d", len(statuses2))
	}
	if statuses2[0].Enabled {
		t.Error("server should be disabled")
	}
	if statuses2[0].Connected {
		t.Error("server should be disconnected")
	}

	t.Logf("disabled server status: %+v", statuses2[0])
}

// ============================================================================
// 测试 7：LLM 端到端 — 中文时事搜索
// ============================================================================

func TestIntegration_MCP_LLM_RealSearch(t *testing.T) {
	skipIfShort(t)
	skipIfNoLLM(t)
	log.Logger = zap.NewNop().Sugar()

	mgr := setupIntegrationMCP(t)

	toolMgr := tools.NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	if err := RegisterTools(toolMgr, mgr); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	mcpTools, err := toolMgr.ResolveTools(ctx, &tools.ToolSessionContext{})
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}

	maxTokens := 4096
	prov := openai.New(
		openai.WithAPIKey(integLLMAPIKey),
		openai.WithBaseURL(integLLMBaseURL),
		openai.WithChatMode(),
		openai.WithChatPath(integLLMChatPath),
		openai.WithTimeout(90*time.Second),
	)

	result, err := llm.OrchestrateGenerate(ctx, prov, &llm.OrchestrateConfig{
		MaxSteps: 5,
		Params: llm.GenerateParams{
			Model: llm.ChatModel(integLLMModel),
			System: fmt.Sprintf(
				"你是一个互联网搜索助手。使用工具搜索互联网并基于搜索结果回答问题。"+
					"可用的搜索工具名称以 %s__ 开头。", integMCPServerName,
			),
			Messages: []llm.Message{
				llm.UserMessage("帮我搜索一下今天有什么科技新闻？"),
			},
			Tools:     mcpTools,
			MaxTokens: &maxTokens,
		},
	})
	if err != nil {
		t.Fatalf("OrchestrateGenerate: %v", err)
	}

	// 验证多步执行
	if len(result.Steps) < 2 {
		t.Errorf("expected at least 2 steps (call + final), got %d", len(result.Steps))
	}

	// 验证工具被调用
	toolCallCount := 0
	for _, step := range result.Steps {
		for _, tc := range step.ToolCalls {
			toolCallCount++
			t.Logf("tool call: %s", tc.ToolName)
		}
	}
	if toolCallCount == 0 {
		t.Error("expected at least 1 tool call across steps")
	}

	// 验证最终回复包含实质内容
	if len(result.Text) < 20 {
		t.Errorf("final text too short: %q", result.Text)
	}

	preview := result.Text
	if len(preview) > 800 {
		preview = preview[:800] + "..."
	}
	t.Logf("final response (%d steps, %d tool calls):\n%s",
		len(result.Steps), toolCallCount, preview)
	t.Logf("tokens: input=%d output=%d total=%d",
		result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.TotalTokens)
}
